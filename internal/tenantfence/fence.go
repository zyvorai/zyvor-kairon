// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package tenantfence turns spec.tenant into an enforceable boundary.
//
// Kairon still does not own BPF. A Machine opts in with the annotation
// kairon.zyvor.dev/tenant-fence=true. The controller projects the tenant
// label, and for each opted-in namespace/tenant pair it owns a
// NetworkSecurityGroup whose denyCidrs are the other tenants' known
// guest addresses (/32 or /128). kairon-node merges those CIDRs into the
// policy it already posts to FluxVM, so a user MachineNetworkPolicy is
// not replaced.
//
// This is east-west deny of addresses Kubernetes has already observed.
// It is not a VRF. A guest that has not yet reported an address, or that
// talks to an address outside this namespace's Machine status, is not
// covered.
package tenantfence

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	// LabelTenant is the selector key projected from spec.tenant.
	LabelTenant = "kairon.zyvor.dev/tenant"
	// AnnFence opts a Machine into fence generation. Any other value is off.
	AnnFence = "kairon.zyvor.dev/tenant-fence"
	// AnnRequireTenant matches agentplane: admission rejects an empty tenant.
	AnnRequireTenant = "kairon.zyvor.dev/require-tenant"
	// LabelManaged marks groups this controller owns and may delete.
	LabelManaged = "kairon.zyvor.dev/tenant-fence"
	// ManagedValue is the only LabelManaged value the controller deletes.
	ManagedValue = "managed"
	// GroupPrefix keeps generated names out of the user name space.
	GroupPrefix = "tenant-fence-"
	// GroupPriority is high so a user group with the default priority
	// does not sort ahead of the fence by accident. FluxVM still applies
	// the merged deny list itself; priority is documentary.
	GroupPriority = 1000
)

var tenantName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// View is the Machine slice the controller already has, narrowed to what
// fence generation reads. Deleting Machines contribute neither opt-in nor
// a deny address: their guest IP is about to disappear.
type View struct {
	Namespace   string
	Name        string
	Tenant      string
	GuestIP     string
	GuestIPs    []string
	Annotations map[string]string
	Deleting    bool
}

// DesiredGroup is one controller-owned NetworkSecurityGroup.
type DesiredGroup struct {
	Namespace   string
	Name        string
	Tenant      string
	Description string
	DenyCIDRs   []string
}

// GroupPatch is a merge-patch for an existing managed group whose deny
// list drifted.
type GroupPatch struct {
	Namespace string
	Name      string
	Patch     map[string]any
}

// GroupKey identifies a group to delete.
type GroupKey struct {
	Namespace string
	Name      string
}

// WantsFence reports whether this Machine asked for a tenant fence.
func WantsFence(annotations map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(annotations[AnnFence]), "true")
}

// ValidTenant is the DNS-1123 label rule used for spec.tenant when it is
// set. Empty is not valid here; callers decide whether empty is allowed.
func ValidTenant(tenant string) error {
	if tenant == "" {
		return fmt.Errorf("tenant is required")
	}
	if !tenantName.MatchString(tenant) {
		return fmt.Errorf("tenant %q must match %s", tenant, tenantName.String())
	}
	return nil
}

// CheckCreate rejects a bad tenant, and an empty tenant when the Machine
// opted into the fence or require-tenant. A tenant-less Machine with
// neither annotation stays valid so existing clusters do not break.
func CheckCreate(tenant string, annotations map[string]string) error {
	if tenant == "" {
		if WantsFence(annotations) || truthy(annotations[AnnRequireTenant]) {
			return fmt.Errorf("spec.tenant: tenant is required")
		}
		return nil
	}
	if err := ValidTenant(tenant); err != nil {
		return fmt.Errorf("spec.tenant: %w", err)
	}
	return nil
}

// CheckUpdate rejects a tenant change or clear. spec.tenant is identity
// for the fence; renaming it would leave the old group's selector and
// the new Machine's denies pointing at different tenants.
func CheckUpdate(oldTenant, newTenant string, annotations map[string]string) error {
	if err := CheckCreate(newTenant, annotations); err != nil {
		return err
	}
	if oldTenant != newTenant {
		return fmt.Errorf("spec.tenant is immutable (was %q, now %q)", oldTenant, newTenant)
	}
	return nil
}

// CheckPolicy refuses a MachineNetworkPolicy whose selector names a
// tenant the policy object itself is not labeled with. A policy with no
// tenant selector is untouched. This stops a tenant-scoped writer from
// attaching a policy to another tenant's Machines.
func CheckPolicy(selector, labels map[string]string) error {
	want, ok := selector[LabelTenant]
	if !ok {
		return nil
	}
	if err := ValidTenant(want); err != nil {
		return fmt.Errorf("spec.selector[%s]: %w", LabelTenant, err)
	}
	got := labels[LabelTenant]
	if got == "" {
		return fmt.Errorf("spec.selector[%s]=%q requires metadata.labels[%s]=%q", LabelTenant, want, LabelTenant, want)
	}
	if got != want {
		return fmt.Errorf("spec.selector[%s]=%q does not match metadata.labels[%s]=%q", LabelTenant, want, LabelTenant, got)
	}
	return nil
}

// NeedsLabel reports whether metadata.labels must be patched to match
// spec.tenant. An empty tenant clears a previously projected label.
func NeedsLabel(labels map[string]string, tenant string) bool {
	got := ""
	if labels != nil {
		got = labels[LabelTenant]
	}
	return got != tenant
}

// LabelPatch is a strategic-merge fragment. A nil value clears the key.
func LabelPatch(tenant string) map[string]any {
	var value any = tenant
	if tenant == "" {
		value = nil
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{LabelTenant: value},
		},
	}
}

// Desired builds one group per namespace/tenant that has at least one
// non-deleting Machine opted into the fence. DenyCIDRs are the other
// tenants' guest addresses in that same namespace. An address that also
// appears on a same-tenant Machine is not denied.
func Desired(views []View) []DesiredGroup {
	type key struct{ ns, tenant string }
	opted := map[key]struct{}{}
	byNS := map[string][]View{}
	for _, v := range views {
		if v.Deleting || strings.TrimSpace(v.Namespace) == "" {
			continue
		}
		byNS[v.Namespace] = append(byNS[v.Namespace], v)
		if v.Tenant == "" || !WantsFence(v.Annotations) {
			continue
		}
		if ValidTenant(v.Tenant) != nil {
			continue
		}
		opted[key{v.Namespace, v.Tenant}] = struct{}{}
	}
	keys := make([]key, 0, len(opted))
	for k := range opted {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ns != keys[j].ns {
			return keys[i].ns < keys[j].ns
		}
		return keys[i].tenant < keys[j].tenant
	})
	out := make([]DesiredGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, DesiredGroup{
			Namespace:   k.ns,
			Name:        GroupPrefix + k.tenant,
			Tenant:      k.tenant,
			Description: fmt.Sprintf("kairon tenant fence for %s/%s: deny guest addresses observed on other tenants in this namespace", k.ns, k.tenant),
			DenyCIDRs:   denyCIDRs(k.tenant, byNS[k.ns]),
		})
	}
	return out
}

// Plan diffs desired groups against the live list. Only objects with
// LabelManaged=managed are updated or deleted. A user group that happens
// to share the generated name is left alone and reported as skipped via
// the create path being omitted — the caller logs Create's AlreadyExists.
func Plan(desired []DesiredGroup, existing []model.NetworkSecurityGroup) (create []model.NetworkSecurityGroup, patches []GroupPatch, del []GroupKey) {
	want := map[string]DesiredGroup{}
	for _, d := range desired {
		want[d.Namespace+"/"+d.Name] = d
		create = append(create, object(d))
	}
	for _, g := range existing {
		if g.Metadata.Labels[LabelManaged] != ManagedValue {
			continue
		}
		id := g.Namespace() + "/" + g.Metadata.Name
		d, ok := want[id]
		if !ok {
			del = append(del, GroupKey{Namespace: g.Namespace(), Name: g.Metadata.Name})
			continue
		}
		delete(want, id)
		if !sameDeny(g.Spec.Policy.DenyCidrs, d.DenyCIDRs) || g.Spec.Description != d.Description || g.Spec.Priority != GroupPriority || g.Spec.Policy.DefaultAllow {
			patches = append(patches, GroupPatch{Namespace: d.Namespace, Name: d.Name, Patch: specPatch(d)})
		}
	}
	filtered := create[:0]
	for _, obj := range create {
		if _, ok := want[obj.Namespace()+"/"+obj.Metadata.Name]; ok {
			filtered = append(filtered, obj)
		}
	}
	return filtered, patches, del
}

// DenyIndex indexes managed fence groups by namespace and tenant.
func DenyIndex(groups []model.NetworkSecurityGroup) map[string][]string {
	out := map[string][]string{}
	for _, g := range groups {
		if g.Metadata.Labels[LabelManaged] != ManagedValue {
			continue
		}
		tenant := g.Metadata.Labels[LabelTenant]
		if tenant == "" {
			continue
		}
		key := g.Namespace() + "\x00" + tenant
		out[key] = append([]string(nil), g.Spec.Policy.DenyCidrs...)
	}
	return out
}

// Merge unions deny CIDRs into a copy of p. User denies are kept.
// DefaultAllow is not modified. Order is sorted so a read-back compare
// is stable.
func Merge(p model.VmNetworkPolicy, deny []string) model.VmNetworkPolicy {
	if len(deny) == 0 {
		return p
	}
	seen := map[string]struct{}{}
	merged := make([]string, 0, len(p.DenyCidrs)+len(deny))
	for _, c := range append(append([]string{}, p.DenyCidrs...), deny...) {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		merged = append(merged, c)
	}
	sort.Strings(merged)
	p.DenyCidrs = merged
	return p
}

func object(d DesiredGroup) model.NetworkSecurityGroup {
	return model.NetworkSecurityGroup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: "NetworkSecurityGroup"},
		Metadata: model.ObjectMeta{
			Name:      d.Name,
			Namespace: d.Namespace,
			Labels: map[string]string{
				LabelManaged: ManagedValue,
				LabelTenant:  d.Tenant,
			},
		},
		Spec: model.NetworkSecurityGroupSpec{
			GroupName:   d.Name,
			Description: d.Description,
			Priority:    GroupPriority,
			Labels:      []string{"tenant=" + d.Tenant, "managed=kairon-tenant-fence"},
			Policy: model.VmNetworkPolicy{
				DefaultAllow: true,
				DenyCidrs:    append([]string(nil), d.DenyCIDRs...),
			},
		},
	}
}

func specPatch(d DesiredGroup) map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"description": d.Description,
			"priority":    GroupPriority,
			"policy": map[string]any{
				"defaultAllow": true,
				"denyCidrs":    d.DenyCIDRs,
			},
		},
	}
}

func denyCIDRs(tenant string, peers []View) []string {
	own := map[string]struct{}{}
	foreign := map[string]struct{}{}
	for _, p := range peers {
		if p.Deleting {
			continue
		}
		addrs := guestCIDRs(p)
		if p.Tenant == tenant {
			for _, a := range addrs {
				own[a] = struct{}{}
			}
			continue
		}
		if p.Tenant == "" {
			continue
		}
		for _, a := range addrs {
			foreign[a] = struct{}{}
		}
	}
	out := make([]string, 0, len(foreign))
	for a := range foreign {
		if _, shared := own[a]; shared {
			continue
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func guestCIDRs(v View) []string {
	raw := make([]string, 0, 1+len(v.GuestIPs))
	if v.GuestIP != "" {
		raw = append(raw, v.GuestIP)
	}
	raw = append(raw, v.GuestIPs...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		c, ok := hostCIDR(s)
		if !ok {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

func hostCIDR(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return "", false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32", true
	}
	return ip.String() + "/128", true
}

func sameDeny(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes":
		return true
	default:
		return false
	}
}
