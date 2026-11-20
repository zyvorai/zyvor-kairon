// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"fmt"
	"reflect"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileResourceLimits applies spec.resources.limits (and, once the
// scheduler has made an allocation, spec.resources.allocatedCpuSet) to the
// live FluxVM cgroup via FluxVM's backend-agnostic
// POST /v1/vms/{id}/resources -- see model.ResourceLimits's own doc
// comment for why this is a separate, freely-raisable-or-lowerable field
// from the grow-only hotplug reconcileHotplug already handles. The two
// are merged into one call rather than two separate FluxVM requests, so
// a tick that changes both never sends conflicting/overlapping writes to
// the same endpoint. Returns the limits actually applied (unchanged from
// status.appliedResourceLimits on a no-op or a failure), so the caller
// can record it the same way reconcileHotplug's own return values are
// recorded -- status.appliedResourceLimits.cpuSetCpus doubles as
// confirmation the cpuset write landed, no separate status field needed.
func (a *Agent) reconcileResourceLimits(ctx context.Context, m model.Machine, rec *fluxvm.Record) (*model.ResourceLimits, error) {
	var limits model.ResourceLimits
	if m.Spec.Resources.Limits != nil {
		limits = *m.Spec.Resources.Limits
	}
	limits.CPUSetCPUs = m.Spec.Resources.AllocatedCPUSet
	if m.Spec.Resources.Limits == nil && len(limits.CPUSetCPUs) == 0 {
		// Nothing requested -- also nothing to clear: FluxVM's own
		// ResourcePatch only ever touches fields explicitly set in the
		// call, so there is no "reset to unset" support to fall back to
		// even if Kairon wanted one. status.appliedResourceLimits (if any,
		// from a previous spec that has since removed the field) is left
		// exactly as it was, honestly reflecting the last limits actually
		// enforced on the cgroup rather than pretending they've been lifted.
		return m.Status.AppliedResourceLimits, nil
	}
	if m.Status.AppliedResourceLimits != nil && reflect.DeepEqual(limits, *m.Status.AppliedResourceLimits) {
		return &limits, nil
	}
	if err := a.Flux.SetResourceLimits(ctx, rec.ID(), limits); err != nil {
		return m.Status.AppliedResourceLimits, fmt.Errorf("set resource limits: %w", err)
	}
	return &limits, nil
}
