// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	// DefaultMemoryEvacuateGrace is MemoryPressureEvacuation.Grace's
	// fallback when unset.
	DefaultMemoryEvacuateGrace = 2 * time.Minute
	// DefaultMemoryEvacuateMinRetryInterval is
	// MemoryPressureEvacuation.MinRetryInterval's fallback when unset.
	DefaultMemoryEvacuateMinRetryInterval = 5 * time.Minute
)

// MemoryPressureEvacuation makes kairon-controller migrate Machines off a
// node whose KaironMemoryPressure condition has been True for Grace:
// kairon-node sets it when host memory stays under pressure after every
// elastic-memory Machine is ballooned to its minimum. Off by default, like
// CordonEvacuation, since enabling it moves Machines with nobody watching.
type MemoryPressureEvacuation struct {
	Enabled bool
	// Strategy mirrors MachineMigrationSpec.Strategy: live|auto|cold.
	// With live, Machines with passthrough devices are skipped, since live
	// migration refuses them.
	Strategy string
	// Grace is how long the condition must stay True before the first
	// migration, so a short spike that ballooning absorbs moves nothing.
	Grace time.Duration
	// MinRetryInterval throttles retries of a Machine a disruption budget
	// blocked or whose migration ended.
	MinRetryInterval time.Duration
}

func (c MemoryPressureEvacuation) grace() time.Duration {
	if c.Grace > 0 {
		return c.Grace
	}
	return DefaultMemoryEvacuateGrace
}

func (c MemoryPressureEvacuation) minRetryInterval() time.Duration {
	if c.MinRetryInterval > 0 {
		return c.MinRetryInterval
	}
	return DefaultMemoryEvacuateMinRetryInterval
}

// memoryPressureSince returns when n's KaironMemoryPressure condition
// turned True, and false when it is not True. A True condition without a
// parseable transition time counts as just turned True.
func memoryPressureSince(n model.Node, now time.Time) (time.Time, bool) {
	for _, c := range n.Status.Conditions {
		if c.Type != model.NodeConditionKaironMemoryPressure {
			continue
		}
		if c.Status != "True" {
			return time.Time{}, false
		}
		if t, err := time.Parse(time.RFC3339, c.LastTransitionTime); err == nil {
			return t, true
		}
		return now, true
	}
	return time.Time{}, false
}

// memoryEvacuationCandidates orders a pressured node's Machines by who
// moves first: lowest spec.priority, then most memory (frees the most per
// migration), then name.
func memoryEvacuationCandidates(machines []model.Machine, node string, live bool) []model.Machine {
	var out []model.Machine
	for _, m := range machines {
		if m.Spec.NodeName != node || m.Metadata.DeletionTimestamp != nil || m.Status.Phase != "Running" {
			continue
		}
		if live && (len(m.Spec.Devices) > 0 || len(m.Spec.DeviceClaims) > 0 || m.Metadata.Annotations[model.AnnotationVFIOBDF] != "") {
			continue
		}
		out = append(out, m)
	}
	mem := func(m model.Machine) uint64 {
		v, _ := model.ParseMemoryMiB(m.Spec.Resources.Memory)
		return v
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Spec.Priority != out[j].Spec.Priority {
			return out[i].Spec.Priority < out[j].Spec.Priority
		}
		if mi, mj := mem(out[i]), mem(out[j]); mi != mj {
			return mi > mj
		}
		return out[i].Namespace()+"/"+out[i].Metadata.Name < out[j].Namespace()+"/"+out[j].Metadata.Name
	})
	return out
}

// reconcileMemoryPressureEvacuation starts at most one MachineMigration per
// pressured node at a time, and none while a migration of a Machine on that
// node is still in flight. Disruption budgets are checked with the same
// LoadBudgetStates/AdmitDisruption decision cordon evacuation uses. The
// scheduler already skips nodes reporting KaironMemoryPressure, so the
// migration lands elsewhere.
func (c *Controller) reconcileMemoryPressureEvacuation(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) {
	if !c.MemoryEvacuation.Enabled {
		return
	}
	now := time.Now().UTC()
	var pressured []string
	for _, n := range nodes {
		if since, ok := memoryPressureSince(n, now); ok && now.Sub(since) >= c.MemoryEvacuation.grace() {
			pressured = append(pressured, n.Metadata.Name)
		}
	}
	if len(pressured) == 0 {
		return
	}
	sort.Strings(pressured)

	nodeOf := map[string]string{}
	for _, m := range machines {
		nodeOf[m.Namespace()+"/"+m.Metadata.Name] = m.Spec.NodeName
	}
	busyNode := map[string]bool{}
	inFlight := map[string]bool{}
	for _, mig := range migrations {
		if cordonEvacuateTerminal(mig.Status.Phase) {
			continue
		}
		key := mig.Namespace() + "/" + mig.Spec.MachineName
		inFlight[key] = true
		busyNode[nodeOf[key]] = true
		if mig.Status.SourceNode != "" {
			busyNode[mig.Status.SourceNode] = true
		}
	}

	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("memory evacuation: list machine disruption budgets failed", "error", err)
		return
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("memory evacuation: load budget states failed", "error", err)
		return
	}

	strategy := c.MemoryEvacuation.Strategy
	if strategy == "" {
		strategy = "live"
	}
	for _, node := range pressured {
		if busyNode[node] {
			continue
		}
		for _, m := range memoryEvacuationCandidates(machines, node, strategy == "live") {
			key := m.Namespace() + "/" + m.Metadata.Name
			if inFlight[key] {
				continue
			}
			if at, ok := m.Metadata.Annotations[model.AnnotationMemoryEvacuateAttemptedAt]; ok {
				if t, err := time.Parse(time.RFC3339, at); err == nil && now.Sub(t) < c.MemoryEvacuation.minRetryInterval() {
					continue
				}
			}
			if blocker := AdmitDisruption(states, m); blocker != "" {
				c.Log.Info("memory evacuation: blocked by disruption budget", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "reason", blocker)
				c.patchMemoryEvacuateAttempt(ctx, m, now)
				continue
			}
			migration := model.MachineMigration{
				TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
				Metadata: model.ObjectMeta{Name: "memory-evacuate-" + node + "-" + m.Metadata.Name + "-" + now.Format("20060102-150405"), Namespace: m.Namespace()},
				Spec:     model.MachineMigrationSpec{MachineName: m.Metadata.Name, Strategy: strategy},
			}
			if _, err := c.Kube.CreateMachineMigration(ctx, m.Namespace(), migration); err != nil {
				c.Log.Error("memory evacuation: create machine migration failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
				c.patchMemoryEvacuateAttempt(ctx, m, now)
				continue
			}
			c.Log.Info("memory evacuation: created machine migration", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "migration", migration.Metadata.Name)
			c.patchMemoryEvacuateAttempt(ctx, m, now)
			break
		}
	}
}

func (c *Controller) patchMemoryEvacuateAttempt(ctx context.Context, m model.Machine, at time.Time) {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		model.AnnotationMemoryEvacuateAttemptedAt: at.Format(time.RFC3339),
	}}}
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		c.Log.Error("memory evacuation: patch attempt annotation failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}
