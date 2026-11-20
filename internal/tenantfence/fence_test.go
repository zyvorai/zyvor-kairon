// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package tenantfence

import (
	"slices"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestCheckCreateAndUpdate(t *testing.T) {
	if err := CheckCreate("", nil); err != nil {
		t.Fatalf("empty tenant without opt-in: %v", err)
	}
	if err := CheckCreate("", map[string]string{AnnFence: "true"}); err == nil {
		t.Fatal("fence without tenant was allowed")
	}
	if err := CheckCreate("Acme", nil); err == nil {
		t.Fatal("uppercase tenant was allowed")
	}
	if err := CheckCreate("acme", map[string]string{AnnFence: "true"}); err != nil {
		t.Fatalf("valid fence tenant: %v", err)
	}
	if err := CheckUpdate("acme", "acme", nil); err != nil {
		t.Fatalf("same tenant: %v", err)
	}
	if err := CheckUpdate("acme", "other", nil); err == nil {
		t.Fatal("tenant rename was allowed")
	}
	if err := CheckUpdate("acme", "", nil); err == nil {
		t.Fatal("tenant clear was allowed")
	}
}

func TestCheckPolicy(t *testing.T) {
	if err := CheckPolicy(map[string]string{"app": "web"}, nil); err != nil {
		t.Fatalf("selector without tenant: %v", err)
	}
	err := CheckPolicy(map[string]string{LabelTenant: "acme"}, map[string]string{LabelTenant: "other"})
	if err == nil {
		t.Fatal("cross-tenant selector was allowed")
	}
	if err := CheckPolicy(map[string]string{LabelTenant: "acme"}, map[string]string{LabelTenant: "acme"}); err != nil {
		t.Fatalf("matching selector: %v", err)
	}
}

func TestDesiredDeniesOtherTenantGuestIPs(t *testing.T) {
	views := []View{
		{Namespace: "lab", Name: "a", Tenant: "acme", GuestIP: "10.0.0.1", Annotations: map[string]string{AnnFence: "true"}},
		{Namespace: "lab", Name: "b", Tenant: "acme", GuestIPs: []string{"10.0.0.2"}},
		{Namespace: "lab", Name: "c", Tenant: "globex", GuestIP: "10.0.0.8", GuestIPs: []string{"2001:db8::8"}},
		{Namespace: "lab", Name: "d", Tenant: "globex", GuestIP: "10.0.0.1"}, // shared with acme, must not be denied
		{Namespace: "lab", Name: "e", Tenant: "initech", GuestIP: "127.0.0.1"},
		{Namespace: "other", Name: "f", Tenant: "globex", GuestIP: "10.9.9.9", Annotations: map[string]string{AnnFence: "true"}},
		{Namespace: "lab", Name: "gone", Tenant: "globex", GuestIP: "10.0.0.7", Deleting: true},
	}
	got := Desired(views)
	if len(got) != 2 {
		t.Fatalf("groups = %+v, want acme in lab and globex in other", got)
	}
	if got[0].Name != "tenant-fence-acme" || got[0].Namespace != "lab" {
		t.Fatalf("first group = %+v", got[0])
	}
	want := []string{"10.0.0.8/32", "2001:db8::8/128"}
	if !slices.Equal(got[0].DenyCIDRs, want) {
		t.Fatalf("acme denies = %v, want %v", got[0].DenyCIDRs, want)
	}
	if len(got[1].DenyCIDRs) != 0 {
		t.Fatalf("other/globex should not see lab addresses, got %v", got[1].DenyCIDRs)
	}
}

func TestPlanCreatePatchDelete(t *testing.T) {
	desired := Desired([]View{
		{Namespace: "lab", Name: "a", Tenant: "acme", GuestIP: "10.0.0.1", Annotations: map[string]string{AnnFence: "true"}},
		{Namespace: "lab", Name: "c", Tenant: "globex", GuestIP: "10.0.0.8"},
	})
	existing := []model.NetworkSecurityGroup{
		{
			Metadata: model.ObjectMeta{Name: "tenant-fence-acme", Namespace: "lab", Labels: map[string]string{LabelManaged: ManagedValue, LabelTenant: "acme"}},
			Spec:     model.NetworkSecurityGroupSpec{Description: "stale", Policy: model.VmNetworkPolicy{DefaultAllow: true, DenyCidrs: []string{"10.9.9.9/32"}}},
		},
		{
			Metadata: model.ObjectMeta{Name: "tenant-fence-old", Namespace: "lab", Labels: map[string]string{LabelManaged: ManagedValue, LabelTenant: "old"}},
		},
		{
			Metadata: model.ObjectMeta{Name: "user-group", Namespace: "lab", Labels: map[string]string{"app": "keep"}},
		},
	}
	create, patches, del := Plan(desired, existing)
	if len(create) != 0 {
		t.Fatalf("create = %+v, want none (acme already exists)", create)
	}
	if len(patches) != 1 || patches[0].Name != "tenant-fence-acme" {
		t.Fatalf("patches = %+v", patches)
	}
	if len(del) != 1 || del[0].Name != "tenant-fence-old" {
		t.Fatalf("delete = %+v", del)
	}
}

func TestMergeKeepsUserDenies(t *testing.T) {
	got := Merge(model.VmNetworkPolicy{DefaultAllow: false, DenyCidrs: []string{"192.0.2.9/32"}}, []string{"10.0.0.8/32", "192.0.2.9/32"})
	if got.DefaultAllow {
		t.Fatal("merge changed DefaultAllow")
	}
	want := []string{"10.0.0.8/32", "192.0.2.9/32"}
	if !slices.Equal(got.DenyCidrs, want) {
		t.Fatalf("merged = %v, want %v", got.DenyCidrs, want)
	}
	idx := DenyIndex([]model.NetworkSecurityGroup{{
		Metadata: model.ObjectMeta{Name: "tenant-fence-acme", Namespace: "lab", Labels: map[string]string{LabelManaged: ManagedValue, LabelTenant: "acme"}},
		Spec:     model.NetworkSecurityGroupSpec{Policy: model.VmNetworkPolicy{DenyCidrs: []string{"10.0.0.8/32"}}},
	}})
	if !slices.Equal(idx["lab\x00acme"], []string{"10.0.0.8/32"}) {
		t.Fatalf("index = %#v", idx)
	}
}

func TestLabelPatchClears(t *testing.T) {
	if !NeedsLabel(map[string]string{LabelTenant: "acme"}, "") {
		t.Fatal("expected clear")
	}
	patch := LabelPatch("")
	labels := patch["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[LabelTenant] != nil {
		t.Fatalf("clear patch = %#v", labels)
	}
}
