// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/preempt"
)

// preemptForPending halts one opted-in lower-priority Machine per
// unschedulable preemptor, then resumes victims whose preemptor has
// released capacity. PDB and in-flight migration both refuse a victim.
// The halt is visible to the scheduler on the next tick; this tick does
// not re-run placement.
func (c *Controller) preemptForPending(ctx context.Context, failed []model.Machine, machines []model.Machine, migrations []model.MachineMigration) {
	migrating := map[string]struct{}{}
	for _, mig := range migrations {
		if isTerminalMigrationPhase(mig.Status.Phase) {
			continue
		}
		migrating[mig.Namespace()+"/"+mig.Spec.MachineName] = struct{}{}
	}
	preemptors := make([]preempt.Machine, 0, len(failed))
	for _, m := range failed {
		preemptors = append(preemptors, preemptView(m))
	}
	views := make([]preempt.Machine, 0, len(machines))
	byKey := map[string]model.Machine{}
	for _, m := range machines {
		views = append(views, preemptView(m))
		byKey[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil {
		c.Log.Error("preempt budget list failed", "error", err)
		budgets = nil
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("preempt budget state failed", "error", err)
		states = nil
	}
	for _, d := range preempt.Plan(preemptors, views, migrating) {
		victim, ok := byKey[d.VictimNamespace+"/"+d.VictimName]
		if !ok {
			continue
		}
		if blocker := AdmitDisruption(states, victim); blocker != "" {
			c.Log.Info("preempt refused by disruption budget", "victim", d.VictimNamespace+"/"+d.VictimName, "preemptor", d.Preemptor, "reason", blocker)
			continue
		}
		if err := c.Kube.PatchMachine(ctx, d.VictimNamespace, d.VictimName, preempt.HaltPatch(d)); err != nil {
			c.Log.Error("preempt halt failed", "victim", d.VictimNamespace+"/"+d.VictimName, "preemptor", d.Preemptor, "error", err)
			continue
		}
		c.Log.Info("preempted machine", "victim", d.VictimNamespace+"/"+d.VictimName, "preemptor", d.Preemptor)
	}
	for _, r := range preempt.ResumePlan(views) {
		if err := c.Kube.PatchMachine(ctx, r.Namespace, r.Name, preempt.ResumePatch(r)); err != nil {
			c.Log.Error("preempt resume failed", "machine", r.Namespace+"/"+r.Name, "error", err)
			continue
		}
		c.Log.Info("resumed preempted machine", "machine", r.Namespace+"/"+r.Name)
	}
}

func preemptView(m model.Machine) preempt.Machine {
	return preempt.Machine{
		Namespace:   m.Namespace(),
		Name:        m.Metadata.Name,
		Tenant:      m.Spec.Tenant,
		Priority:    m.Spec.Priority,
		NodeName:    m.Spec.NodeName,
		PowerState:  m.Spec.PowerState,
		Phase:       m.Status.Phase,
		Annotations: m.Metadata.Annotations,
		Deleting:    m.Metadata.DeletionTimestamp != nil,
	}
}
