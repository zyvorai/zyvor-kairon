// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "time"

const (
	KindMachinePool  = "MachinePool"
	KindMachineClaim = "MachineClaim"
)

// A MachinePool keeps spec.replicas booted, unclaimed Machines; a
// MachineClaim takes one of them over by relabelling it, so a claim binds
// in one reconcile tick instead of waiting for a cold boot. Pool members
// carry LabelMachinePool plus LabelPoolState. Only PoolStateWarm members
// count toward spec.replicas, so the pool refills as soon as one is
// claimed. A claimed Machine keeps LabelMachinePool for provenance and gains
// LabelMachineClaim.
const (
	LabelMachinePool         = "kairon.zyvor.dev/machinepool"
	LabelMachinePoolTemplate = "kairon.zyvor.dev/machinepool-template-hash"
	LabelPoolState           = "kairon.zyvor.dev/pool-state"
	LabelMachineClaim        = "kairon.zyvor.dev/machineclaim"

	PoolStateWarm    = "warm"
	PoolStateClaimed = "claimed"

	// FinalizerMachinePool holds a deleting pool until its warm members are
	// gone. Claimed members belong to their claims and are left alone.
	FinalizerMachinePool = "kairon.zyvor.dev/machinepool-cleanup"
	// FinalizerMachineClaim holds a deleting claim until its Machine has
	// been deleted (reclaimPolicy Delete) or released (Retain).
	FinalizerMachineClaim = "kairon.zyvor.dev/machineclaim-cleanup"

	ReclaimDelete = "Delete"
	ReclaimRetain = "Retain"

	ClaimPending = "Pending"
	ClaimBound   = "Bound"
	ClaimLost    = "Lost"
)

type MachinePool struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta        `json:"metadata"`
	Spec     MachinePoolSpec   `json:"spec"`
	Status   MachinePoolStatus `json:"status,omitempty"`
}

func (p MachinePool) Namespace() string {
	return p.Metadata.Namespace
}

type MachinePoolList struct {
	TypeMeta `json:",inline"`
	Items    []MachinePool `json:"items"`
}

type MachinePoolSpec struct {
	// Replicas is how many warm (unclaimed) Machines to keep. The scale
	// subresource points here, so `kubectl scale machinepool` and an HPA
	// work.
	Replicas int             `json:"replicas"`
	Template MachineTemplate `json:"template"`
	// WarmPowerState is Running (default) or Paused. A Paused pool lets
	// each member boot, waits WarmupSeconds, then pauses it: it holds
	// memory but no CPU, and binding resumes it in milliseconds.
	WarmPowerState string `json:"warmPowerState,omitempty"`
	// WarmupSeconds is how long a member runs before it is paused, so
	// the guest finishes booting first. Default DefaultPoolWarmupSeconds.
	WarmupSeconds int64 `json:"warmupSeconds,omitempty"`
}

const DefaultPoolWarmupSeconds = 30

// Warmup is how long a Paused pool's member runs before it is paused.
func (s MachinePoolSpec) Warmup() time.Duration {
	if s.WarmupSeconds > 0 {
		return time.Duration(s.WarmupSeconds) * time.Second
	}
	return DefaultPoolWarmupSeconds * time.Second
}

// AnnotationLifecycleStart, set when a claim binds a pool member, is where
// spec.lifecycle.maxLifetimeSeconds counts from instead of the Machine's
// creation, which for a pool member was long before anyone used it.
const AnnotationLifecycleStart = "kairon.zyvor.dev/lifecycle-start"

type MachinePoolStatus struct {
	// Replicas counts warm members (booting or Running); ReadyReplicas the
	// Running ones a claim can bind to right now.
	Replicas      int `json:"replicas"`
	ReadyReplicas int `json:"readyReplicas"`
	Claimed       int `json:"claimed"`
	// Selector is the label selector of warm members, for the scale
	// subresource.
	Selector string `json:"selector,omitempty"`
	Message  string `json:"message"`
}

type MachineClaim struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Spec     MachineClaimSpec   `json:"spec"`
	Status   MachineClaimStatus `json:"status,omitempty"`
}

func (c MachineClaim) Namespace() string {
	return c.Metadata.Namespace
}

type MachineClaimList struct {
	TypeMeta `json:",inline"`
	Items    []MachineClaim `json:"items"`
}

type MachineClaimSpec struct {
	// PoolName is a MachinePool in the claim's namespace.
	PoolName string `json:"poolName"`
	// Labels are added to the Machine when it is bound.
	Labels map[string]string `json:"labels,omitempty"`
	// ReclaimPolicy is Delete (default: deleting the claim deletes the
	// Machine) or Retain (the Machine is unlabelled and kept).
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
	// TTLSeconds, when set, deletes the claim this long after it binds,
	// which releases the Machine per ReclaimPolicy.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
	// Egress, when set, confines the bound Machine to these destinations
	// for the claim's lifetime: kairon-controller creates a default-deny
	// MachineNetworkPolicy for it and deletes it on release.
	Egress *ClaimEgress `json:"egress,omitempty"`
}

// ClaimEgress is a per-claim egress allowlist, enforced by the FluxVM VM
// edge through a MachineNetworkPolicy. Anything not listed is dropped.
type ClaimEgress struct {
	AllowCidrs []string `json:"allowCidrs,omitempty"`
	AllowPorts []string `json:"allowPorts,omitempty"`
	AllowFqdns []string `json:"allowFqdns,omitempty"`
	AllowDNS   []string `json:"allowDNS,omitempty"`
	AllowSNI   []string `json:"allowSNI,omitempty"`
	AllowIcmp  bool     `json:"allowIcmp,omitempty"`
}

// Policy is the VM-edge policy a ClaimEgress becomes.
func (e ClaimEgress) Policy() VmNetworkPolicy {
	return VmNetworkPolicy{
		DefaultAllow: false,
		AllowCidrs:   e.AllowCidrs,
		AllowPorts:   e.AllowPorts,
		AllowFqdns:   e.AllowFqdns,
		AllowDNS:     e.AllowDNS,
		AllowSNI:     e.AllowSNI,
		AllowIcmp:    e.AllowIcmp,
	}
}

// ClaimEgressPolicyName is the MachineNetworkPolicy a claim's egress
// allowlist is enforced through.
func ClaimEgressPolicyName(claim string) string {
	name := "claim-egress-" + claim
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

type MachineClaimStatus struct {
	Phase       string     `json:"phase,omitempty"`
	MachineName string     `json:"machineName,omitempty"`
	BoundAt     *time.Time `json:"boundAt,omitempty"`
	// BindMillis is creationTimestamp to bind, in milliseconds.
	BindMillis int64 `json:"bindMillis,omitempty"`
	// EgressPolicy is the MachineNetworkPolicy enforcing spec.egress.
	EgressPolicy string `json:"egressPolicy,omitempty"`
	// Usage is what the bound Machine has consumed since binding, accrued
	// by kairon-controller every ClaimUsageInterval.
	Usage   ClaimUsage `json:"usage,omitempty"`
	Message string     `json:"message"`
}

// ClaimUsage meters a bound claim: seconds its Machine was active
// (Running) and suspended (Paused, Hibernated, Halted, Stopped), plus
// vCPU- and memory-GiB-seconds while active.
type ClaimUsage struct {
	ActiveSeconds    int64   `json:"activeSeconds,omitempty"`
	SuspendedSeconds int64   `json:"suspendedSeconds,omitempty"`
	VCPUSeconds      float64 `json:"vcpuSeconds,omitempty"`
	MemoryGiBSeconds float64 `json:"memoryGiBSeconds,omitempty"`
	// LastAccrued is RFC 3339; accrual resumes from here.
	LastAccrued string `json:"lastAccrued,omitempty"`
}

// ClaimUsageInterval bounds how often a claim's usage is written.
const ClaimUsageInterval = 30 * time.Second

// AccrueClaimUsage adds the time since u.LastAccrued (else since bound)
// to u for a Machine in phase, sized vcpus/memGiB. It returns u unchanged
// until ClaimUsageInterval has passed, and the deltas it added.
func AccrueClaimUsage(u ClaimUsage, bound time.Time, phase string, vcpus, memGiB float64, now time.Time) (out ClaimUsage, active, suspended float64) {
	from := bound
	if t, err := time.Parse(time.RFC3339, u.LastAccrued); err == nil {
		from = t
	}
	if from.IsZero() {
		u.LastAccrued = now.UTC().Format(time.RFC3339)
		return u, 0, 0
	}
	dt := now.Sub(from)
	if dt < ClaimUsageInterval {
		return u, 0, 0
	}
	// Whole seconds, so the remainder carries into the next accrual.
	secs := int64(dt / time.Second)
	switch {
	case phase == "Running":
		u.ActiveSeconds += secs
		u.VCPUSeconds += float64(secs) * vcpus
		u.MemoryGiBSeconds += float64(secs) * memGiB
		active = float64(secs)
	case phase == "Stopped" || Suspended(phase):
		u.SuspendedSeconds += secs
		suspended = float64(secs)
	}
	u.LastAccrued = from.Add(time.Duration(secs) * time.Second).UTC().Format(time.RFC3339)
	return u, active, suspended
}
