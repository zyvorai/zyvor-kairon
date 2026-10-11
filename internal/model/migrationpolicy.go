// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

const KindMigrationPolicy = "MigrationPolicy"

// MigrationPolicy scopes migration bandwidth/concurrency to Machines
// matching Selector, instead of the global, cluster-wide
// migration.maxConcurrentPerNode/maxConcurrentCluster Helm values (which
// still apply to every migration regardless of any MigrationPolicy) --
// Kairon's namespace-scoped equivalent of KubeVirt's own MigrationPolicy
// CRD. Enforced by kairon-controller's existing migration reconcile loop
// (internal/controller/migrationpolicy.go), the same "block into Pending/
// Blocked, not a webhook" shape MachineQuota's own reconcile-loop
// enforcement already uses -- no admission webhook exists or is needed.
type MigrationPolicy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta            `json:"metadata"`
	Spec     MigrationPolicySpec   `json:"spec"`
	Status   MigrationPolicyStatus `json:"status,omitempty"`
}

func (p MigrationPolicy) Namespace() string {
	return p.Metadata.Namespace
}

type MigrationPolicyList struct {
	TypeMeta `json:",inline"`
	Items    []MigrationPolicy `json:"items"`
}

type MigrationPolicySpec struct {
	// Selector matches Machines this policy applies to, same shape and
	// semantics as MachineDisruptionBudget.Spec.Selector (LabelsMatch --
	// empty matches nothing, never "everything").
	Selector map[string]string `json:"selector"`
	// BandwidthMbps, when set, becomes a MachineMigration's own
	// spec.bandwidthMbps the first time a matching Machine's migration is
	// admitted, but only if that MachineMigration didn't already set one
	// explicitly (an explicit per-migration value always wins) -- see
	// internal/controller/migrationpolicy.go. When more than one
	// MigrationPolicy in the namespace matches the same Machine, the
	// first one found (Kubernetes list order, not otherwise sorted)
	// supplies the default -- deliberately not merged/most-restrictive,
	// keeping this a simple default, not a second, competing enforcement
	// layer on top of MaxConcurrent below.
	BandwidthMbps uint64 `json:"bandwidthMbps,omitempty"`
	// MaxConcurrent caps how many non-terminal migrations of Machines
	// matching Selector may run at once, cluster-wide -- independent of,
	// and in addition to, migration.maxConcurrentPerNode/maxConcurrentCluster's
	// own global caps. 0 (the default) is unlimited within this policy's
	// own scope (the global caps still apply regardless).
	MaxConcurrent int `json:"maxConcurrent,omitempty"`

	// Strategy is static (the default: start-time settings only) or
	// adaptive (kairon-node's adaptive engine watches each transfer and
	// throttles, relaxes downtime or switches to post-copy when it stalls).
	// Experimental until the lab migration matrix runs green.
	Strategy string `json:"strategy,omitempty"`
	// MaxDowntimeMs becomes spec.maxDowntimeMs when a migration doesn't
	// set one. Adaptive migrations may relax towards it, never beyond.
	MaxDowntimeMs uint64 `json:"maxDowntimeMs,omitempty"`
	// CompletionTimeoutPerGiB (a Go duration such as "120s") bounds an
	// adaptive migration to guest RAM in GiB times this value before it is
	// aborted with the source left running.
	CompletionTimeoutPerGiB string `json:"completionTimeoutPerGiB,omitempty"`
	// ProgressTimeout is how long remaining RAM may fail to shrink before
	// an adaptive migration counts as stalled.
	ProgressTimeout string                 `json:"progressTimeout,omitempty"`
	AutoConverge    *MigrationAutoConverge `json:"autoConverge,omitempty"`
	PostCopy        *MigrationPostCopy     `json:"postCopy,omitempty"`
	Compression     *MigrationCompression  `json:"compression,omitempty"`
	// Bandwidth supersedes BandwidthMbps, which stays as a deprecated alias
	// for bandwidth.maxMbps.
	Bandwidth *MigrationBandwidth `json:"bandwidth,omitempty"`
}

const (
	MigrationTuningStatic   = "static"
	MigrationTuningAdaptive = "adaptive"

	MigrationCompressionNone = "none"
	MigrationCompressionZlib = "zlib"
	MigrationCompressionZstd = "zstd"
)

// MigrationAutoConverge lets the adaptive engine throttle guest vCPUs when
// the guest dirties memory faster than the link drains it.
type MigrationAutoConverge struct {
	Enabled bool `json:"enabled,omitempty"`
	// DirtyRateThresholdMbps starts throttling once the guest dirty rate
	// reaches this, even before the progress timeout expires. 0 relies on
	// stall detection alone.
	DirtyRateThresholdMbps uint64 `json:"dirtyRateThresholdMbps,omitempty"`
}

// MigrationPostCopy lets the adaptive engine switch a stalled pre-copy
// transfer to post-copy. After the switch the guest runs on the destination
// while pages fault in over the network; losing either node then loses the
// guest, so a failure goes to NeedsRecovery instead of resuming the source.
type MigrationPostCopy struct {
	Enabled bool `json:"enabled,omitempty"`
	// TriggerAfter switches to post-copy once the transfer has run this
	// long (a Go duration), even if other stall remedies are still left.
	TriggerAfter string `json:"triggerAfter,omitempty"`
}

// MigrationCompression compresses multifd channels. Requires multifd.
type MigrationCompression struct {
	Algorithm string `json:"algorithm"`
	// Level is 1-20 for zstd and 1-9 for zlib. 0 uses QEMU's default.
	Level uint8 `json:"level,omitempty"`
}

type MigrationBandwidth struct {
	MaxMbps uint64 `json:"maxMbps,omitempty"`
	// Adaptive starts below MaxMbps and raises bandwidth while throughput
	// keeps up, backing off when it stops.
	Adaptive bool `json:"adaptive,omitempty"`
}

// MaxBandwidthMbps resolves bandwidth.maxMbps with the deprecated
// bandwidthMbps alias as fallback.
func (s MigrationPolicySpec) MaxBandwidthMbps() uint64 {
	if s.Bandwidth != nil && s.Bandwidth.MaxMbps > 0 {
		return s.Bandwidth.MaxMbps
	}
	return s.BandwidthMbps
}

// MigrationPolicyStatus mirrors MachineDisruptionBudget's own status
// shape -- purely observational, written every reconcile tick as a side
// effect of the same tally AdmitMigrationPolicy already computes.
type MigrationPolicyStatus struct {
	ActiveMigrations int `json:"activeMigrations,omitempty"`
}
