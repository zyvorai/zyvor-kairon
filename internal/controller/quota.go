// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// QuotaTracker accumulates one MachineQuota's usage for the duration of a
// single Reconcile pass -- seeded from already-scheduled Machines, then
// spent as newly-admitted Machines are scheduled within the same pass, so a
// burst of Machine creations in one tick can't all slip through before
// status catches up on the next tick. Exported (along with
// BuildQuotaTrackers/AdmitQuotaResize/MachineCountsTowardQuota/
// MachineFootprint below) so internal/agent's own hotplug reconciliation
// can reuse the identical admission logic for its own resize-past-quota
// self-check, independent of whether webhook.enabled is set -- see
// internal/agent/hotplug.go.
type QuotaTracker struct {
	quota model.MachineQuota
	used  model.MachineQuotaStatus
}

// BuildQuotaTrackers seeds one tracker per MachineQuota from every Machine
// already consuming real capacity in that quota's namespace: not deleted,
// not desired-Stopped, and already scheduled (spec.nodeName set). A Machine
// that exists but hasn't been scheduled yet doesn't count until it actually
// gets in -- quota blocks *new* scheduling, it doesn't evict or retroactively
// un-admit anything.
func BuildQuotaTrackers(quotas []model.MachineQuota, machines []model.Machine) (map[string][]*QuotaTracker, error) {
	trackers := map[string][]*QuotaTracker{}
	for _, q := range quotas {
		trackers[q.Namespace()] = append(trackers[q.Namespace()], &QuotaTracker{quota: q})
	}
	for _, m := range machines {
		if !MachineCountsTowardQuota(m) {
			continue
		}
		cpu, mem := MachineFootprint(m)
		for _, t := range trackers[m.Namespace()] {
			t.used.UsedMachines++
			t.used.UsedTotalCPUCores += cpu
			t.used.UsedTotalMemoryMiB += mem
		}
	}
	for _, list := range trackers {
		for _, t := range list {
			if t.quota.Spec.MaxTotalCPU != "" {
				if _, err := model.ParseVCPUs(t.quota.Spec.MaxTotalCPU); err != nil {
					return nil, fmt.Errorf("MachineQuota %s/%s: invalid maxTotalCpu: %w", t.quota.Namespace(), t.quota.Metadata.Name, err)
				}
			}
			if t.quota.Spec.MaxTotalMemory != "" {
				if _, err := model.ParseMemoryMiB(t.quota.Spec.MaxTotalMemory); err != nil {
					return nil, fmt.Errorf("MachineQuota %s/%s: invalid maxTotalMemory: %w", t.quota.Namespace(), t.quota.Metadata.Name, err)
				}
			}
		}
	}
	return trackers, nil
}

// MachineFootprint returns m's own CPU/memory footprint as MachineQuota
// accounts for it.
func MachineFootprint(m model.Machine) (cpu uint32, memMiB uint64) {
	cpu, _ = model.ParseVCPUs(m.Spec.Resources.CPU)
	memMiB, _ = model.ParseMemoryMiB(m.Spec.Resources.Memory)
	return cpu, memMiB
}

// MachineCountsTowardQuota is the one predicate for "does this Machine's
// footprint currently count against its namespace's MachineQuota" --
// shared by BuildQuotaTrackers' seed pass and the admission webhook's
// UPDATE handling (see AdmitQuotaResize) so the two can never drift apart
// on what "already counted" means. Matches quota blocking *new*
// scheduling, not evicting or retroactively un-admitting anything: not
// deleted, not desired-Stopped-or-Halted (both genuinely free the
// runtime/host footprint the same way, see countAssigned's own comment
// in internal/controller/controller.go), and already scheduled.
func MachineCountsTowardQuota(m model.Machine) bool {
	desired := m.DesiredPowerState()
	return m.Metadata.DeletionTimestamp == nil && !model.PowerStateFreesCapacity(desired) && m.Spec.NodeName != ""
}

// admitQuota returns a non-empty blocker reason if scheduling m would push
// any MachineQuota it's subject to over a limit, otherwise it spends m's
// footprint against every quota in its namespace and returns "".
func admitQuota(trackers map[string][]*QuotaTracker, m model.Machine) string {
	cpu, mem := MachineFootprint(m)
	for _, t := range trackers[m.Namespace()] {
		if t.quota.Spec.MaxMachines != nil && t.used.UsedMachines+1 > *t.quota.Spec.MaxMachines {
			return fmt.Sprintf("MachineQuota %s/%s: maxMachines %d reached", t.quota.Namespace(), t.quota.Metadata.Name, *t.quota.Spec.MaxMachines)
		}
		if t.quota.Spec.MaxTotalCPU != "" {
			max, _ := model.ParseVCPUs(t.quota.Spec.MaxTotalCPU) // already validated in buildQuotaTrackers
			if t.used.UsedTotalCPUCores+cpu > max {
				return fmt.Sprintf("MachineQuota %s/%s: maxTotalCpu %q reached", t.quota.Namespace(), t.quota.Metadata.Name, t.quota.Spec.MaxTotalCPU)
			}
		}
		if t.quota.Spec.MaxTotalMemory != "" {
			max, _ := model.ParseMemoryMiB(t.quota.Spec.MaxTotalMemory) // already validated in buildQuotaTrackers
			if t.used.UsedTotalMemoryMiB+mem > max {
				return fmt.Sprintf("MachineQuota %s/%s: maxTotalMemory %q reached", t.quota.Namespace(), t.quota.Metadata.Name, t.quota.Spec.MaxTotalMemory)
			}
		}
	}
	for _, t := range trackers[m.Namespace()] {
		t.used.UsedMachines++
		t.used.UsedTotalCPUCores += cpu
		t.used.UsedTotalMemoryMiB += mem
	}
	return ""
}

// AdmitQuotaResize returns a non-empty blocker reason if growing an
// already-scheduled Machine's spec.resources from old to new would push
// any MachineQuota it's subject to over its CPU/memory limit -- the
// UPDATE counterpart to admitQuota's CREATE check (see
// validateMachine in webhook.go). Also reused directly by
// internal/agent/hotplug.go for kairon-node's own resize-past-quota
// self-check, independent of webhook.enabled. Deliberately never touches
// MaxMachines: a resize doesn't change how many Machines exist. old must
// be old's already-counted footprint (i.e. the caller has confirmed
// MachineCountsTowardQuota(oldMachine) so trackers already includes it,
// seeded by BuildQuotaTrackers from the pre-update Machine list) --
// subtracting it before adding new is what makes this check the delta,
// not admitQuota's "add a brand-new footprint on top" one. A shrink
// (new <= old on both dimensions) can never be denied here: it only ever
// lowers usage below what BuildQuotaTrackers already saw and accepted.
func AdmitQuotaResize(trackers map[string][]*QuotaTracker, namespace string, oldCPU, newCPU uint32, oldMemMiB, newMemMiB uint64) string {
	for _, t := range trackers[namespace] {
		if t.quota.Spec.MaxTotalCPU != "" {
			max, _ := model.ParseVCPUs(t.quota.Spec.MaxTotalCPU) // already validated in buildQuotaTrackers
			if t.used.UsedTotalCPUCores-oldCPU+newCPU > max {
				return fmt.Sprintf("MachineQuota %s/%s: maxTotalCpu %q reached", t.quota.Namespace(), t.quota.Metadata.Name, t.quota.Spec.MaxTotalCPU)
			}
		}
		if t.quota.Spec.MaxTotalMemory != "" {
			max, _ := model.ParseMemoryMiB(t.quota.Spec.MaxTotalMemory) // already validated in buildQuotaTrackers
			if t.used.UsedTotalMemoryMiB-oldMemMiB+newMemMiB > max {
				return fmt.Sprintf("MachineQuota %s/%s: maxTotalMemory %q reached", t.quota.Namespace(), t.quota.Metadata.Name, t.quota.Spec.MaxTotalMemory)
			}
		}
	}
	return ""
}

// QuotaTrackersForNamespace lists MachineQuotas and Machines for
// namespace and seeds trackers from them -- the shared I/O sequence
// behind the admission webhook's validateMachineCreate/validateMachineResize
// and internal/agent/hotplug.go's own resize-past-quota self-check
// (independent of whether webhook.enabled is set). ok is false only when
// the caller should short-circuit straight to "allow"/"no check needed"
// without denying anything: no MachineQuota exists in this namespace at
// all, so there's nothing to enforce and no reason to pay for the extra
// ListMachines call.
func QuotaTrackersForNamespace(ctx context.Context, kc *kube.Client, namespace string) (trackers map[string][]*QuotaTracker, ok bool, err error) {
	quotas, err := kc.ListMachineQuotasNamespace(ctx, namespace)
	if err != nil {
		return nil, false, fmt.Errorf("list MachineQuotas: %w", err)
	}
	if len(quotas) == 0 {
		return nil, false, nil
	}
	machines, err := kc.ListMachinesNamespace(ctx, namespace)
	if err != nil {
		return nil, false, fmt.Errorf("list Machines: %w", err)
	}
	trackers, err = BuildQuotaTrackers(quotas, machines)
	if err != nil {
		return nil, false, err
	}
	return trackers, true, nil
}
