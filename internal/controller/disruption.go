// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// BudgetState tracks one MachineDisruptionBudget's remaining allowance for
// the duration of a single caller -- either one `kaironctl evacuate` run
// (cmd/kaironctl/main.go's cmdEvacuate) or one admission-webhook request
// (webhook.go's validateMachineMigration). Recomputed from scratch every
// time, never persisted: nothing reconciles this object on a timer (see
// docs/guides/machine-disruption-budgets.md). isTerminalMigrationPhase here
// deliberately duplicates rather than imports internal/uiapi's own
// isNonTerminalMigrationPhase -- same "each consumer's own narrower
// definition of terminal" precedent that helper's own comment establishes,
// not a shared cross-package dependency worth taking for one boolean.
type BudgetState struct {
	budget  model.MachineDisruptionBudget
	total   int
	healthy int
	desired int
	allowed int
}

// Status snapshots this budget's MachineDisruptionBudgetStatus as of when
// LoadBudgetStates computed it -- unaffected by any later AdmitDisruption
// calls against allowed (those track a caller's own spend-as-you-go
// allowance for one evacuate/webhook run, not the budget's observed
// state). Used by reconcileDisruptionBudgetsStatus to patch the object's
// real status once per reconcile tick.
func (s *BudgetState) Status() model.MachineDisruptionBudgetStatus {
	return model.MachineDisruptionBudgetStatus{
		ExpectedMachines:   s.total,
		CurrentHealthy:     s.healthy,
		DesiredHealthy:     s.desired,
		DisruptionsAllowed: s.allowed,
	}
}

// Budget returns the MachineDisruptionBudget this state was computed for.
func (s *BudgetState) Budget() model.MachineDisruptionBudget {
	return s.budget
}

func isTerminalMigrationPhase(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Blocked", "Cancelled", "":
		return true
	}
	return false
}

// LoadBudgetStates computes each budget's remaining allowance from the
// current Machines/MachineMigrations that already exist -- callers pass in
// their own freshly-listed data (this does no I/O itself), then pass the
// result to AdmitDisruption once per Machine they're about to disrupt.
func LoadBudgetStates(budgets []model.MachineDisruptionBudget, machines []model.Machine, migrations []model.MachineMigration) ([]*BudgetState, error) {
	inFlight := map[string]bool{}
	for _, mig := range migrations {
		if !isTerminalMigrationPhase(mig.Status.Phase) {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}
	states := make([]*BudgetState, 0, len(budgets))
	for _, b := range budgets {
		total, healthy := 0, 0
		for _, m := range machines {
			if !model.LabelsMatch(m.Metadata.Labels, b.Spec.Selector) {
				continue
			}
			total++
			if m.Status.Phase == "Running" && !inFlight[m.Namespace()+"/"+m.Metadata.Name] {
				healthy++
			}
		}
		desired, err := b.Spec.DesiredHealthy(total)
		if err != nil {
			return nil, fmt.Errorf("MachineDisruptionBudget %s/%s: %w", b.Namespace(), b.Metadata.Name, err)
		}
		allowed := healthy - desired
		if allowed < 0 {
			allowed = 0
		}
		states = append(states, &BudgetState{budget: b, total: total, healthy: healthy, desired: desired, allowed: allowed})
	}
	return states, nil
}

// AdmitDisruption returns a non-empty reason if disrupting machine would
// violate some budget it matches, otherwise it spends one allowance from
// every budget machine matches and returns "".
func AdmitDisruption(states []*BudgetState, machine model.Machine) string {
	var applicable []*BudgetState
	for _, st := range states {
		if model.LabelsMatch(machine.Metadata.Labels, st.budget.Spec.Selector) {
			if st.allowed <= 0 {
				return fmt.Sprintf("MachineDisruptionBudget %s/%s has 0 disruptions allowed", st.budget.Namespace(), st.budget.Metadata.Name)
			}
			applicable = append(applicable, st)
		}
	}
	for _, st := range applicable {
		st.allowed--
	}
	return ""
}

// reconcileDisruptionBudgetsStatus lists every MachineDisruptionBudget
// cluster-wide and patches its status from a fresh LoadBudgetStates
// computation against the machines/migrations this tick already listed --
// no separate I/O for machines/migrations, this is purely a status-visibility
// pass, it never blocks or mutates a Machine/MachineMigration itself. A
// LoadBudgetStates error (a malformed minAvailable/maxUnavailable) is
// returned so the caller logs it once rather than per-budget; an
// individual PatchMachineDisruptionBudgetStatus failure is logged and
// skipped so one bad write doesn't stop every other budget's status from
// updating.
//
// observed carries each budget with its freshly-computed Status() attached
// -- exactly the same values PatchMachineDisruptionBudgetStatus above wrote
// to the real object -- and is handed to Metrics.ObserveDisruptionBudgets
// so kairon_disruption_budget_status never reports numbers that disagree
// with what `kubectl get machinedisruptionbudget` would show for the same
// tick, same pattern as controller.go's observedQuotas/ObserveQuotas. A
// budget whose status patch itself failed is still observed with the
// computed (not the possibly-stale-in-etcd) status, since the metric's job
// is to reflect what Kairon just computed, not to second-guess whether the
// write landed.
func (c *Controller) reconcileDisruptionBudgetsStatus(ctx context.Context, machines []model.Machine, migrations []model.MachineMigration) error {
	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		return err
	}
	observed := make([]model.MachineDisruptionBudget, 0, len(states))
	for _, st := range states {
		b := st.Budget()
		b.Status = st.Status()
		observed = append(observed, b)
		if statusErr := c.Kube.PatchMachineDisruptionBudgetStatus(ctx, b.Namespace(), b.Metadata.Name, b.Status); statusErr != nil {
			c.Log.Error("machine disruption budget status patch failed", "namespace", b.Namespace(), "budget", b.Metadata.Name, "error", statusErr)
		}
	}
	if c.Metrics != nil {
		c.Metrics.ObserveDisruptionBudgets(observed)
	}
	return nil
}
