// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// MigrationPolicyState tracks one MigrationPolicy's current active-migration
// count against its own MaxConcurrent cap -- mirrors BudgetState
// (disruption.go) exactly, same "recomputed from scratch every Reconcile
// tick, never persisted" shape.
type MigrationPolicyState struct {
	policy model.MigrationPolicy
	active int
}

// LoadMigrationPolicyStates computes each policy's current active-migration
// count from the machines/migrations this tick already listed -- no
// separate I/O, mirrors LoadBudgetStates.
func LoadMigrationPolicyStates(policies []model.MigrationPolicy, machines []model.Machine, migrations []model.MachineMigration) []*MigrationPolicyState {
	machineByKey := map[string]model.Machine{}
	for _, m := range machines {
		machineByKey[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	states := make([]*MigrationPolicyState, 0, len(policies))
	for _, p := range policies {
		active := 0
		for _, mig := range migrations {
			if mig.Namespace() != p.Namespace() || !isActiveMigrationPhase(mig.Status.Phase) {
				continue
			}
			m, ok := machineByKey[mig.Namespace()+"/"+mig.Spec.MachineName]
			if !ok || !model.LabelsMatch(m.Metadata.Labels, p.Spec.Selector) {
				continue
			}
			active++
		}
		states = append(states, &MigrationPolicyState{policy: p, active: active})
	}
	return states
}

// AdmitMigrationPolicy returns a non-empty blocker if starting a
// migration for machine would push some matching MigrationPolicy's
// MaxConcurrent over its cap, otherwise it spends one slot from every
// policy machine matches and returns "" -- mirrors AdmitDisruption
// exactly.
func AdmitMigrationPolicy(states []*MigrationPolicyState, machine model.Machine) string {
	var applicable []*MigrationPolicyState
	for _, st := range states {
		if machine.Namespace() != st.policy.Namespace() || !model.LabelsMatch(machine.Metadata.Labels, st.policy.Spec.Selector) {
			continue
		}
		if st.policy.Spec.MaxConcurrent > 0 && st.active >= st.policy.Spec.MaxConcurrent {
			return "MigrationPolicy " + st.policy.Namespace() + "/" + st.policy.Metadata.Name + ": maxConcurrent reached"
		}
		applicable = append(applicable, st)
	}
	for _, st := range applicable {
		st.active++
	}
	return ""
}

// MigrationTuning is what admission snapshots from MigrationPolicies onto
// a MachineMigration.
type MigrationTuning struct {
	BandwidthMbps uint64
	MaxDowntimeMs uint64
	// Adaptive is set when a matching policy has strategy adaptive.
	Adaptive *model.MachineMigrationAdaptiveSpec
}

// ResolveMigrationTuning resolves each setting from the first matching
// MigrationPolicy (list order, not otherwise sorted) that sets it --
// deliberately "first match," not merged or most-restrictive, keeping
// policies a simple default rather than a second enforcement layer on top
// of MaxConcurrent. Exported so kaironctl's `describe migrationpolicy`
// preview computes the identical answer.
func ResolveMigrationTuning(states []*MigrationPolicyState, machine model.Machine) MigrationTuning {
	var t MigrationTuning
	for _, st := range states {
		p := st.policy
		if machine.Namespace() != p.Namespace() || !model.LabelsMatch(machine.Metadata.Labels, p.Spec.Selector) {
			continue
		}
		if t.BandwidthMbps == 0 {
			t.BandwidthMbps = p.Spec.MaxBandwidthMbps()
		}
		if t.MaxDowntimeMs == 0 {
			t.MaxDowntimeMs = p.Spec.MaxDowntimeMs
		}
		if t.Adaptive == nil && p.Spec.Strategy == model.MigrationTuningAdaptive {
			t.Adaptive = &model.MachineMigrationAdaptiveSpec{
				Policy:                  p.Metadata.Name,
				CompletionTimeoutPerGiB: p.Spec.CompletionTimeoutPerGiB,
				ProgressTimeout:         p.Spec.ProgressTimeout,
				AutoConverge:            p.Spec.AutoConverge,
				PostCopy:                p.Spec.PostCopy,
				Compression:             p.Spec.Compression,
				Bandwidth:               p.Spec.Bandwidth,
			}
			if t.Adaptive.Bandwidth == nil && p.Spec.BandwidthMbps > 0 {
				t.Adaptive.Bandwidth = &model.MigrationBandwidth{MaxMbps: p.Spec.BandwidthMbps}
			}
		}
	}
	return t
}

// BandwidthMbpsFromPolicies returns the bandwidth ResolveMigrationTuning
// would default a migration of machine to, or 0.
func BandwidthMbpsFromPolicies(states []*MigrationPolicyState, machine model.Machine) uint64 {
	return ResolveMigrationTuning(states, machine).BandwidthMbps
}

// defaultCompressionMultifdChannels is used when compression is
// snapshotted onto a migration that didn't ask for multifd, since QEMU only
// compresses multifd channels.
const defaultCompressionMultifdChannels = 2

// MigrationTuningPatch returns the spec merge patch that applies t to spec,
// or nil when there is nothing to change. Every value the migration set
// explicitly wins, field by field, including inside spec.adaptive.
func MigrationTuningPatch(spec model.MachineMigrationSpec, t MigrationTuning) (map[string]any, error) {
	patch := map[string]any{}
	adaptive := mergeAdaptiveSpec(spec.Adaptive, t.Adaptive)
	if adaptive != nil {
		if err := ValidateAdaptiveSpec(*adaptive); err != nil {
			return nil, err
		}
		if spec.Adaptive == nil || *adaptive != *spec.Adaptive {
			patch["adaptive"] = adaptive
		}
		if c := adaptive.Compression; c != nil && c.Algorithm != model.MigrationCompressionNone && spec.MultifdChannels == 0 {
			patch["multifdChannels"] = defaultCompressionMultifdChannels
		}
	}
	if spec.BandwidthMbps == 0 {
		bw := t.BandwidthMbps
		if adaptive != nil && adaptive.Bandwidth != nil && adaptive.Bandwidth.MaxMbps > 0 {
			bw = adaptive.Bandwidth.MaxMbps
			if adaptive.Bandwidth.Adaptive {
				bw = max(bw/4, 1)
			}
		}
		if bw > 0 {
			patch["bandwidthMbps"] = bw
		}
	}
	if spec.MaxDowntimeMs == 0 && t.MaxDowntimeMs > 0 {
		patch["maxDowntimeMs"] = t.MaxDowntimeMs
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, nil
}

func mergeAdaptiveSpec(explicit, policy *model.MachineMigrationAdaptiveSpec) *model.MachineMigrationAdaptiveSpec {
	if explicit == nil && policy == nil {
		return nil
	}
	if explicit == nil {
		out := *policy
		return &out
	}
	out := *explicit
	if policy == nil {
		return &out
	}
	if out.Policy == "" {
		out.Policy = policy.Policy
	}
	if out.CompletionTimeoutPerGiB == "" {
		out.CompletionTimeoutPerGiB = policy.CompletionTimeoutPerGiB
	}
	if out.ProgressTimeout == "" {
		out.ProgressTimeout = policy.ProgressTimeout
	}
	if out.AutoConverge == nil {
		out.AutoConverge = policy.AutoConverge
	}
	if out.PostCopy == nil {
		out.PostCopy = policy.PostCopy
	}
	if out.Compression == nil {
		out.Compression = policy.Compression
	}
	if out.Bandwidth == nil {
		out.Bandwidth = policy.Bandwidth
	}
	return &out
}

// ValidateAdaptiveSpec checks what the CRD schema can't: Go duration syntax.
func ValidateAdaptiveSpec(a model.MachineMigrationAdaptiveSpec) error {
	for name, v := range map[string]string{
		"completionTimeoutPerGiB": a.CompletionTimeoutPerGiB,
		"progressTimeout":         a.ProgressTimeout,
		"postCopy.triggerAfter":   triggerAfter(a.PostCopy),
	} {
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("adaptive %s %q must be a positive duration such as 30s", name, v)
		}
	}
	if c := a.Compression; c != nil {
		switch c.Algorithm {
		case model.MigrationCompressionNone, model.MigrationCompressionZlib, model.MigrationCompressionZstd:
		default:
			return fmt.Errorf("adaptive compression algorithm %q must be none, zlib or zstd", c.Algorithm)
		}
		if (c.Algorithm == model.MigrationCompressionZstd && c.Level > 20) || (c.Algorithm == model.MigrationCompressionZlib && c.Level > 9) {
			return fmt.Errorf("adaptive compression level %d is out of range for %s", c.Level, c.Algorithm)
		}
	}
	return nil
}

func triggerAfter(p *model.MigrationPostCopy) string {
	if p == nil {
		return ""
	}
	return p.TriggerAfter
}

// loadMigrationPolicyStates lists every MigrationPolicy and computes its
// current state -- called once per Reconcile tick, before the migration
// admission loop, so AdmitMigrationPolicy/BandwidthMbpsFromPolicies see
// (and spend against) the same states every reconcileMigration call this
// tick shares. Returns nil (not an error) if the CRD isn't installed --
// same tolerant-of-absence convention every other optional CRD kind in
// this reconcile loop already follows.
func (c *Controller) loadMigrationPolicyStates(ctx context.Context, machines []model.Machine, migrations []model.MachineMigration) []*MigrationPolicyState {
	policies, err := c.Kube.ListMigrationPolicies(ctx)
	if err != nil {
		if !kube.IsNotFound(err) {
			c.Log.Error("list migration policies failed", "error", err)
		}
		return nil
	}
	return LoadMigrationPolicyStates(policies, machines, migrations)
}

// patchMigrationPolicyStatuses persists each policy's active-migration
// count as of the end of this tick's migration admission loop -- purely
// observational, mirrors reconcileDisruptionBudgetsStatus's own status-only
// patch pass.
func (c *Controller) patchMigrationPolicyStatuses(ctx context.Context, states []*MigrationPolicyState) {
	for _, st := range states {
		status := model.MigrationPolicyStatus{ActiveMigrations: st.active}
		if err := c.Kube.PatchMigrationPolicyStatus(ctx, st.policy.Namespace(), st.policy.Metadata.Name, status); err != nil {
			c.Log.Error("migration policy status patch failed", "namespace", st.policy.Namespace(), "policy", st.policy.Metadata.Name, "error", err)
		}
	}
}
