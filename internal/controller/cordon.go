// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// DefaultCordonEvacuateMinRetryInterval is CordonEvacuation.MinRetryInterval's
// fallback when unset.
const DefaultCordonEvacuateMinRetryInterval = 2 * time.Minute

// CordonEvacuation makes kairon-controller create a MachineMigration for
// every Machine on a Node whose spec.unschedulable transitions to true --
// kaironctl evacuate NODE, automatically, the same way KubeVirt's
// LiveMigrateIfPossible eviction strategy reacts to a cordon/drain. Off
// by default: unlike leaderElection's default-on (a provable no-op at
// replicaCount 1), this is never a no-op -- enabling it means a cordoned
// node's Machines start migrating on their own, without an operator
// running `kaironctl evacuate` or `kubectl drain --force`-adjacent
// tooling triggering it. See docs/guides/machine-disruption-budgets.md.
// A node in maintenance mode is left to NodeMaintenance (maintenance.go).
type CordonEvacuation struct {
	Enabled bool
	// Strategy mirrors MachineMigrationSpec.Strategy: cold|auto. Live is
	// deliberately not offered here -- automatic *live* migration
	// triggered with no operator watching is a bigger step than
	// automatic cold, and effectiveStrategy's own "auto stays
	// conservative" reasoning (internal/agent) already treats auto as
	// the safer default for exactly this kind of unattended trigger.
	Strategy string
	// MinRetryInterval throttles how often a Machine blocked by a
	// MachineDisruptionBudget is retried -- without it, a controller
	// reconciling every few seconds would attempt a new MachineMigration
	// every single tick for any Machine a budget is currently blocking.
	MinRetryInterval time.Duration
}

func (c CordonEvacuation) minRetryInterval() time.Duration {
	if c.MinRetryInterval > 0 {
		return c.MinRetryInterval
	}
	return DefaultCordonEvacuateMinRetryInterval
}

// cordonEvacuateTerminal mirrors this project's own per-consumer
// "terminal migration phase" precedent (disruption.go's
// isTerminalMigrationPhase, cmd/kaironctl's migrationStillPending) --
// each caller keeps its own narrower definition rather than sharing one,
// per that function's own doc comment.
func cordonEvacuateTerminal(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Blocked", "Cancelled":
		return true
	}
	return false
}

// reconcileCordonEvacuation creates a MachineMigration for every Machine
// on a newly-cordoned node, reusing the exact LoadBudgetStates/
// AdmitDisruption decision `kaironctl evacuate`/the admission webhook
// already share -- never a new disruption-budget check. Uses the
// machines/nodes/migrations this tick's Reconcile already listed; only
// I/O of its own is listing MachineDisruptionBudgets (already a separate
// list call in reconcileDisruptionBudgetsStatus -- not shared, since that
// function computes its own states from the same inputs for an unrelated
// purpose, and duplicating one cheap list call is simpler than
// restructuring both around a shared cache for this first cut) and the
// CreateMachineMigration/PatchMachine calls it actually decides to make.
func (c *Controller) reconcileCordonEvacuation(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) {
	if !c.CordonEvacuation.Enabled {
		return
	}
	cordoned := map[string]bool{}
	for _, n := range nodes {
		if n.Spec.Unschedulable && !model.MaintenanceRequested(n) {
			cordoned[n.Metadata.Name] = true
		}
	}
	if len(cordoned) == 0 {
		return
	}

	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("cordon evacuation: list machine disruption budgets failed", "error", err)
		return
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("cordon evacuation: load budget states failed", "error", err)
		return
	}
	inFlight := map[string]bool{}
	for _, mig := range migrations {
		if !cordonEvacuateTerminal(mig.Status.Phase) {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}

	now := time.Now().UTC()
	for _, m := range machines {
		if m.Metadata.DeletionTimestamp != nil || !cordoned[m.Spec.NodeName] {
			continue
		}
		key := m.Namespace() + "/" + m.Metadata.Name
		if inFlight[key] {
			continue
		}
		if attemptedAt, ok := m.Metadata.Annotations[model.AnnotationCordonEvacuateAttemptedAt]; ok {
			if t, err := time.Parse(time.RFC3339, attemptedAt); err == nil && now.Sub(t) < c.CordonEvacuation.minRetryInterval() {
				continue
			}
		}
		if blocker := AdmitDisruption(states, m); blocker != "" {
			c.Log.Info("cordon evacuation: blocked by disruption budget", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", m.Spec.NodeName, "reason", blocker)
			c.patchCordonEvacuateAttempt(ctx, m, now)
			continue
		}
		migration := model.MachineMigration{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
			Metadata: model.ObjectMeta{Name: cordonEvacuateMigrationName(m.Spec.NodeName, m.Metadata.Name, now), Namespace: m.Namespace()},
			Spec:     model.MachineMigrationSpec{MachineName: m.Metadata.Name, Strategy: c.CordonEvacuation.Strategy},
		}
		if _, err := c.Kube.CreateMachineMigration(ctx, m.Namespace(), migration); err != nil {
			c.Log.Error("cordon evacuation: create machine migration failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			continue
		}
		c.Log.Info("cordon evacuation: created machine migration", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", m.Spec.NodeName, "migration", migration.Metadata.Name)
		c.patchCordonEvacuateAttempt(ctx, m, now)
	}
}

func (c *Controller) patchCordonEvacuateAttempt(ctx context.Context, m model.Machine, at time.Time) {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		model.AnnotationCordonEvacuateAttemptedAt: at.Format(time.RFC3339),
	}}}
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		c.Log.Error("cordon evacuation: patch attempt annotation failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}

func cordonEvacuateMigrationName(node, machine string, at time.Time) string {
	return "cordon-evacuate-" + node + "-" + machine + "-" + at.Format("20060102-150405")
}
