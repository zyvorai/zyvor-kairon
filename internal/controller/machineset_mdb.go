// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// MachineSet scale-down honors MachineDisruptionBudgets.
//
// HPA, KEDA and `kubectl scale` only lower spec.replicas through the /scale
// subresource; the MachineSet controller then picks which replicas to
// delete. Before this, that delete ignored every MachineDisruptionBudget,
// so an autoscaler could take a set below its budget's minAvailable in one
// tick. Now each healthy replica removed for scale-down must first be
// admitted by the same LoadBudgetStates/AdmitDisruption logic
// `kaironctl evacuate`, the admission webhook and the fleet
// MachineAutoscaler (fleet.go) already use. A replica the budget refuses is
// kept, status.conditions gets ScaleDownBlocked=True (reason
// DisruptionBudget) plus one Warning Event, and the next reconcile tick
// retries from a fresh listing -- there is no separate requeue queue in
// this controller, the periodic tick is the requeue.
//
// Scope: only excess current-template replicas (a pure scale-down).
// RollingUpdate/Recreate replacement of outdated replicas keeps its own
// maxUnavailable/maxSurge pacing and is not gated here.

const (
	machineSetConditionScaleDownBlocked = "ScaleDownBlocked"
	machineSetReasonDisruptionBudget    = "DisruptionBudget"
	machineSetReasonScaleDownAllowed    = "ScaleDownAllowed"
)

// scaleDownBlockedError is returned by scaleDownMachineSet when a budget
// held back at least one replica. reconcileMachineSet turns it into the
// ScaleDownBlocked condition rather than a reconcile failure.
type scaleDownBlockedError struct{ reason string }

func (e *scaleDownBlockedError) Error() string { return "scale-down blocked: " + e.reason }

// machineSetBudgetCache loads budgets and migrations at most once per
// reconcile tick, and keeps one set of BudgetStates per namespace so two
// MachineSets covered by the same budget spend from one shared allowance
// within a tick instead of each seeing the full allowance.
type machineSetBudgetCache struct {
	machines    []model.Machine
	listed      bool
	budgets     []model.MachineDisruptionBudget
	migrations  []model.MachineMigration
	byNamespace map[string][]*BudgetState
}

type machineSetBudgetCacheKey struct{}

// withMachineSetBudgetCache attaches a per-tick cache to ctx. Carried on
// the context rather than as a parameter so reconcileMachineSet/
// stepMachineSetToward keep their signatures.
func withMachineSetBudgetCache(ctx context.Context, machines []model.Machine) context.Context {
	return context.WithValue(ctx, machineSetBudgetCacheKey{}, &machineSetBudgetCache{machines: machines, byNamespace: map[string][]*BudgetState{}})
}

// machineSetBudgetStates returns the budget states for ns. Budgets are
// namespaced like a PodDisruptionBudget: only budgets and Machines in ns
// count. Without a tick cache on ctx (a direct call, e.g. from a test) it
// falls back to a one-off cache over fallback, so a scale-down is never
// silently ungated. A missing CRD (NotFound) means no budgets; any other
// list error fails closed by returning it.
func (c *Controller) machineSetBudgetStates(ctx context.Context, ns string, fallback []model.Machine) ([]*BudgetState, error) {
	cache, _ := ctx.Value(machineSetBudgetCacheKey{}).(*machineSetBudgetCache)
	if cache == nil {
		cache = &machineSetBudgetCache{machines: fallback, byNamespace: map[string][]*BudgetState{}}
	}
	if states, ok := cache.byNamespace[ns]; ok {
		return states, nil
	}
	if !cache.listed {
		budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
		if err != nil && !kube.IsNotFound(err) {
			return nil, fmt.Errorf("list machine disruption budgets: %w", err)
		}
		cache.budgets = budgets
		if len(budgets) > 0 {
			migrations, err := c.Kube.ListMachineMigrations(ctx)
			if err != nil && !kube.IsNotFound(err) {
				return nil, fmt.Errorf("list machine migrations: %w", err)
			}
			cache.migrations = migrations
		}
		cache.listed = true
	}
	var budgets []model.MachineDisruptionBudget
	for _, b := range cache.budgets {
		if b.Namespace() == ns {
			budgets = append(budgets, b)
		}
	}
	var machines []model.Machine
	for _, m := range cache.machines {
		if m.Namespace() == ns {
			machines = append(machines, m)
		}
	}
	states, err := LoadBudgetStates(budgets, machines, cache.migrations)
	if err != nil {
		return nil, err
	}
	cache.byNamespace[ns] = states
	return states, nil
}

// selectScaleDownVictims picks up to n of candidates to delete. Replicas
// that are not Running go first and never spend budget allowance: the
// budget only counts Running Machines as healthy, so removing one cannot
// lower currentHealthy. Each Running replica must be admitted by
// AdmitDisruption, which spends one allowance from every budget it
// matches. blocked is the first refusal reason when fewer than n were
// admitted, "" otherwise.
func selectScaleDownVictims(candidates []model.Machine, n int, states []*BudgetState) (victims []model.Machine, blocked string) {
	ordered := append([]model.Machine(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Status.Phase != "Running" && ordered[j].Status.Phase == "Running"
	})
	for _, m := range ordered {
		if len(victims) == n {
			break
		}
		if m.Status.Phase != "Running" {
			victims = append(victims, m)
			continue
		}
		if reason := AdmitDisruption(states, m); reason != "" {
			if blocked == "" {
				blocked = reason
			}
			continue
		}
		victims = append(victims, m)
	}
	if len(victims) == n {
		blocked = ""
	}
	return victims, blocked
}

// scaleDownMachineSet deletes n of current (excess current-template
// replicas), honoring MachineDisruptionBudgets. It deletes whatever the
// budgets admit and returns *scaleDownBlockedError if any were held back.
func (c *Controller) scaleDownMachineSet(ctx context.Context, ms model.MachineSet, current []model.Machine, n int) error {
	states, err := c.machineSetBudgetStates(ctx, ms.Namespace(), current)
	if err != nil {
		return err
	}
	victims, blocked := selectScaleDownVictims(current, n, states)
	if err := c.deleteMachines(ctx, ms, victims); err != nil {
		return err
	}
	if blocked != "" {
		return &scaleDownBlockedError{reason: fmt.Sprintf("%s; deleted %d of %d excess replicas, retrying next reconcile", blocked, len(victims), n)}
	}
	return nil
}

// machineSetScaleDownCondition records the outcome of this tick's step:
// True with the budget's reason when a scale-down was held back, False
// otherwise. A Warning Event is recorded only when the condition turns
// True (not every tick it stays True) to keep Event history quiet while an
// autoscaler waits on a budget.
func (c *Controller) machineSetScaleDownCondition(ctx context.Context, ms model.MachineSet, conditions []model.Condition, blocked *scaleDownBlockedError) []model.Condition {
	if blocked == nil {
		return model.SetCondition(conditions, model.Condition{Type: machineSetConditionScaleDownBlocked, Status: "False", Reason: machineSetReasonScaleDownAllowed})
	}
	if prev, ok := model.FindCondition(conditions, machineSetConditionScaleDownBlocked); !ok || prev.Status != "True" {
		if err := c.Kube.RecordEvent(ctx, ms.Namespace(), model.KindMachineSet, ms.Metadata.Name, ms.Metadata.UID, machineSetConditionScaleDownBlocked, blocked.reason, "Warning"); err != nil {
			c.Log.Error("machineset event failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", err)
		}
	}
	return model.SetCondition(conditions, model.Condition{Type: machineSetConditionScaleDownBlocked, Status: "True", Reason: machineSetReasonDisruptionBudget, Message: blocked.reason})
}
