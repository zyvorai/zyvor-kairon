// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestListAndPatchMachine(t *testing.T) {
	var patched bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"}}}})
		case r.Method == http.MethodPatch:
			patched = r.Header.Get("Content-Type") == "application/merge-patch+json"
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()
	items, err := c.ListMachines(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if err := c.PatchMachine(context.Background(), "default", "vm1", map[string]any{"spec": map[string]any{"nodeName": "n1"}}); err != nil {
		t.Fatal(err)
	}
	if !patched {
		t.Fatal("expected merge patch")
	}
}

func TestListMachineInstanceTypesNamespaceAndListMigrationPoliciesNamespace(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machineinstancetypes":
			_ = json.NewEncoder(w).Encode(model.MachineInstanceTypeList{Items: []model.MachineInstanceType{{Metadata: model.ObjectMeta{Name: "large", Namespace: "prod"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/migrationpolicies":
			_ = json.NewEncoder(w).Encode(model.MigrationPolicyList{Items: []model.MigrationPolicy{{Metadata: model.ObjectMeta{Name: "default", Namespace: "prod"}}}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()

	types, err := c.ListMachineInstanceTypesNamespace(context.Background(), "prod")
	if err != nil || len(types) != 1 || types[0].Metadata.Name != "large" {
		t.Fatalf("types=%v err=%v", types, err)
	}
	policies, err := c.ListMigrationPoliciesNamespace(context.Background(), "prod")
	if err != nil || len(policies) != 1 || policies[0].Metadata.Name != "default" {
		t.Fatalf("policies=%v err=%v", policies, err)
	}
}

func TestListMachineNetworkPoliciesNamespaceAndListNetworkSecurityGroupsNamespace(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinenetworkpolicies":
			_ = json.NewEncoder(w).Encode(model.MachineNetworkPolicyList{Items: []model.MachineNetworkPolicy{{Metadata: model.ObjectMeta{Name: "web-edge", Namespace: "prod"}}}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/networksecuritygroups":
			_ = json.NewEncoder(w).Encode(model.NetworkSecurityGroupList{Items: []model.NetworkSecurityGroup{{Metadata: model.ObjectMeta{Name: "frontend", Namespace: "prod"}}}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()

	policies, err := c.ListMachineNetworkPoliciesNamespace(context.Background(), "prod")
	if err != nil || len(policies) != 1 || policies[0].Metadata.Name != "web-edge" {
		t.Fatalf("policies=%v err=%v", policies, err)
	}
	groups, err := c.ListNetworkSecurityGroupsNamespace(context.Background(), "prod")
	if err != nil || len(groups) != 1 || groups[0].Metadata.Name != "frontend" {
		t.Fatalf("groups=%v err=%v", groups, err)
	}
}

func TestLeaseCreateGetUpdateAndConflict(t *testing.T) {
	const wantPath = "/apis/coordination.k8s.io/v1/namespaces/kairon-system/leases/kairon-controller"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/coordination.k8s.io/v1/namespaces/kairon-system/leases":
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			l.Metadata.ResourceVersion = "1"
			_ = json.NewEncoder(w).Encode(l)
		case r.Method == http.MethodGet && r.URL.Path == wantPath:
			holder := "a"
			_ = json.NewEncoder(w).Encode(model.Lease{
				Metadata: model.ObjectMeta{Name: "kairon-controller", Namespace: "kairon-system", ResourceVersion: "1"},
				Spec:     model.LeaseSpec{HolderIdentity: &holder},
			})
		case r.Method == http.MethodPut && r.URL.Path == wantPath:
			var l model.Lease
			_ = json.NewDecoder(r.Body).Decode(&l)
			if l.Metadata.ResourceVersion != "1" {
				http.Error(w, "conflict", http.StatusConflict)
				return
			}
			l.Metadata.ResourceVersion = "2"
			_ = json.NewEncoder(w).Encode(l)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()
	ctx := context.Background()

	created, err := c.CreateLease(ctx, "kairon-system", model.Lease{Metadata: model.ObjectMeta{Name: "kairon-controller", Namespace: "kairon-system"}})
	if err != nil || created.Metadata.ResourceVersion != "1" {
		t.Fatalf("CreateLease: created=%+v err=%v", created, err)
	}

	got, err := c.GetLease(ctx, "kairon-system", "kairon-controller")
	if err != nil || got.Metadata.ResourceVersion != "1" {
		t.Fatalf("GetLease: got=%+v err=%v", got, err)
	}

	// A stale resourceVersion is rejected with 409, surfaced via IsConflict.
	stale := got
	stale.Metadata.ResourceVersion = "0"
	if _, err := c.UpdateLease(ctx, "kairon-system", stale); !IsConflict(err) {
		t.Fatalf("UpdateLease with stale resourceVersion: err=%v, want a conflict", err)
	}

	updated, err := c.UpdateLease(ctx, "kairon-system", got)
	if err != nil || updated.Metadata.ResourceVersion != "2" {
		t.Fatalf("UpdateLease: updated=%+v err=%v", updated, err)
	}
}

func TestObserveCalledForEverySuccessfulAndFailedRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_ = json.NewEncoder(w).Encode(model.MachineList{})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()

	var calls []struct {
		method  string
		errored bool
	}
	c.Observe = func(method string, d time.Duration, err error) {
		if d < 0 {
			t.Errorf("negative duration observed: %v", d)
		}
		calls = append(calls, struct {
			method  string
			errored bool
		}{method, err != nil})
	}

	_ = c.request(context.Background(), http.MethodGet, "/ok", nil, nil, "")
	_ = c.request(context.Background(), http.MethodGet, "/fail", nil, nil, "")

	if len(calls) != 2 {
		t.Fatalf("Observe called %d times, want 2", len(calls))
	}
	if calls[0].method != http.MethodGet || calls[0].errored {
		t.Errorf("call[0] = %+v, want ok GET", calls[0])
	}
	if calls[1].method != http.MethodGet || !calls[1].errored {
		t.Errorf("call[1] = %+v, want errored GET", calls[1])
	}
}

func TestGetAndDeleteEveryDescribeDeleteEligibleResourceKind(t *testing.T) {
	var deleted []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch r.URL.Path {
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations/mig1":
			_ = json.NewEncoder(w).Encode(model.MachineMigration{Metadata: model.ObjectMeta{Name: "mig1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshots/snap1":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshot{Metadata: model.ObjectMeta{Name: "snap1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesnapshotrestores/restore1":
			_ = json.NewEncoder(w).Encode(model.MachineSnapshotRestore{Metadata: model.ObjectMeta{Name: "restore1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas/quota1":
			_ = json.NewEncoder(w).Encode(model.MachineQuota{Metadata: model.ObjectMeta{Name: "quota1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinedisruptionbudgets/budget1":
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudget{Metadata: model.ObjectMeta{Name: "budget1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/set1":
			_ = json.NewEncoder(w).Encode(model.MachineSet{Metadata: model.ObjectMeta{Name: "set1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machineinstancetypes/type1":
			_ = json.NewEncoder(w).Encode(model.MachineInstanceType{Metadata: model.ObjectMeta{Name: "type1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/migrationpolicies/policy1":
			_ = json.NewEncoder(w).Encode(model.MigrationPolicy{Metadata: model.ObjectMeta{Name: "policy1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinenetworkpolicies/netpol1":
			_ = json.NewEncoder(w).Encode(model.MachineNetworkPolicy{Metadata: model.ObjectMeta{Name: "netpol1", Namespace: "prod"}})
		case "/apis/kairon.zyvor.dev/v1/namespaces/prod/networksecuritygroups/group1":
			_ = json.NewEncoder(w).Encode(model.NetworkSecurityGroup{Metadata: model.ObjectMeta{Name: "group1", Namespace: "prod"}})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()
	ctx := context.Background()

	if got, err := c.GetMachineMigration(ctx, "prod", "mig1"); err != nil || got.Metadata.Name != "mig1" {
		t.Fatalf("GetMachineMigration: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineSnapshot(ctx, "prod", "snap1"); err != nil || got.Metadata.Name != "snap1" {
		t.Fatalf("GetMachineSnapshot: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineSnapshotRestore(ctx, "prod", "restore1"); err != nil || got.Metadata.Name != "restore1" {
		t.Fatalf("GetMachineSnapshotRestore: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineQuota(ctx, "prod", "quota1"); err != nil || got.Metadata.Name != "quota1" {
		t.Fatalf("GetMachineQuota: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineDisruptionBudget(ctx, "prod", "budget1"); err != nil || got.Metadata.Name != "budget1" {
		t.Fatalf("GetMachineDisruptionBudget: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineSet(ctx, "prod", "set1"); err != nil || got.Metadata.Name != "set1" {
		t.Fatalf("GetMachineSet: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineInstanceType(ctx, "prod", "type1"); err != nil || got.Metadata.Name != "type1" {
		t.Fatalf("GetMachineInstanceType: got=%v err=%v", got, err)
	}
	if got, err := c.GetMigrationPolicy(ctx, "prod", "policy1"); err != nil || got.Metadata.Name != "policy1" {
		t.Fatalf("GetMigrationPolicy: got=%v err=%v", got, err)
	}
	if got, err := c.GetMachineNetworkPolicy(ctx, "prod", "netpol1"); err != nil || got.Metadata.Name != "netpol1" {
		t.Fatalf("GetMachineNetworkPolicy: got=%v err=%v", got, err)
	}
	if got, err := c.GetNetworkSecurityGroup(ctx, "prod", "group1"); err != nil || got.Metadata.Name != "group1" {
		t.Fatalf("GetNetworkSecurityGroup: got=%v err=%v", got, err)
	}

	if err := c.DeleteMachineMigration(ctx, "prod", "mig1"); err != nil {
		t.Fatalf("DeleteMachineMigration: %v", err)
	}
	if err := c.DeleteMachineSnapshot(ctx, "prod", "snap1"); err != nil {
		t.Fatalf("DeleteMachineSnapshot: %v", err)
	}
	if err := c.DeleteMachineSnapshotRestore(ctx, "prod", "restore1"); err != nil {
		t.Fatalf("DeleteMachineSnapshotRestore: %v", err)
	}
	if err := c.DeleteMachineQuota(ctx, "prod", "quota1"); err != nil {
		t.Fatalf("DeleteMachineQuota: %v", err)
	}
	if err := c.DeleteMachineDisruptionBudget(ctx, "prod", "budget1"); err != nil {
		t.Fatalf("DeleteMachineDisruptionBudget: %v", err)
	}
	if err := c.DeleteMachineSet(ctx, "prod", "set1"); err != nil {
		t.Fatalf("DeleteMachineSet: %v", err)
	}
	if err := c.DeleteMachineInstanceType(ctx, "prod", "type1"); err != nil {
		t.Fatalf("DeleteMachineInstanceType: %v", err)
	}
	if err := c.DeleteMigrationPolicy(ctx, "prod", "policy1"); err != nil {
		t.Fatalf("DeleteMigrationPolicy: %v", err)
	}
	if err := c.DeleteMachineNetworkPolicy(ctx, "prod", "netpol1"); err != nil {
		t.Fatalf("DeleteMachineNetworkPolicy: %v", err)
	}
	if err := c.DeleteNetworkSecurityGroup(ctx, "prod", "group1"); err != nil {
		t.Fatalf("DeleteNetworkSecurityGroup: %v", err)
	}
	if len(deleted) != 10 {
		t.Fatalf("deleted=%v, want 10 DELETE calls", deleted)
	}
}

// TestRecordEventPostsRealCoreV1Event confirms RecordEvent's request shape
// against a fake apiserver double: method, path, and a well-formed
// core/v1 Event body carrying involvedObject/reason/message/type, with a
// generated, non-empty metadata.name (no caller-supplied one) so repeated
// calls for the same object never collide.
func TestRecordEventPostsRealCoreV1Event(t *testing.T) {
	var gotMethod, gotPath string
	var gotContentType string
	var ev model.Event
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()

	err := c.RecordEvent(context.Background(), "prod", "Machine", "vm-1", "vm-1-uid", "QuotaBlocked", "MachineQuota prod-quota: max machines (5) reached", "Warning")
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/prod/events" {
		t.Errorf("path = %q, want /api/v1/namespaces/prod/events", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if ev.APIVersion != "v1" || ev.Kind != "Event" {
		t.Errorf("unexpected TypeMeta: %+v", ev.TypeMeta)
	}
	if ev.Metadata.Namespace != "prod" {
		t.Errorf("metadata.namespace = %q, want prod", ev.Metadata.Namespace)
	}
	if ev.Metadata.Name == "" {
		t.Error("expected a generated, non-empty metadata.name")
	}
	if ev.InvolvedObject.Kind != "Machine" || ev.InvolvedObject.Name != "vm-1" || ev.InvolvedObject.Namespace != "prod" || ev.InvolvedObject.UID != "vm-1-uid" {
		t.Errorf("unexpected involvedObject: %+v", ev.InvolvedObject)
	}
	if ev.Reason != "QuotaBlocked" {
		t.Errorf("reason = %q, want QuotaBlocked", ev.Reason)
	}
	if ev.Message != "MachineQuota prod-quota: max machines (5) reached" {
		t.Errorf("unexpected message: %q", ev.Message)
	}
	if ev.Type != "Warning" {
		t.Errorf("type = %q, want Warning", ev.Type)
	}
	if ev.Count != 1 {
		t.Errorf("count = %d, want 1", ev.Count)
	}
	if ev.FirstTimestamp.IsZero() || ev.LastTimestamp.IsZero() {
		t.Error("expected non-zero first/last timestamps")
	}

	// A second call for the same involved object must not collide on
	// metadata.name.
	firstName := ev.Metadata.Name
	if err := c.RecordEvent(context.Background(), "prod", "Machine", "vm-1", "vm-1-uid", "QuotaBlocked", "still blocked", "Warning"); err != nil {
		t.Fatalf("second RecordEvent: %v", err)
	}
	if ev.Metadata.Name == firstName {
		t.Errorf("expected a distinct generated metadata.name on the second call, got the same %q twice", firstName)
	}
}

func TestIsTransient(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer slow.Close()
	defer close(block)
	kc, _ := New(slow.URL, "", "", false)
	kc.HTTP = slow.Client()
	kc.HTTP.Timeout = 50 * time.Millisecond
	_, timeoutErr := kc.GetMachine(context.Background(), "prod", "db")

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"503":     {&APIError{StatusCode: http.StatusServiceUnavailable}, true},
		"429":     {&APIError{StatusCode: http.StatusTooManyRequests}, true},
		"404":     {&APIError{StatusCode: http.StatusNotFound}, false},
		"409":     {&APIError{StatusCode: http.StatusConflict}, false},
		"timeout": {timeoutErr, true},
		"plain":   {context.Canceled, false},
	} {
		if got := IsTransient(tc.err); got != tc.want {
			t.Errorf("%s: IsTransient(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}
