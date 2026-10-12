// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// DRS modes.
const (
	DRSModeOff       = "off"
	DRSModeRecommend = "recommend"
	DRSModeAutomatic = "automatic"
)

const (
	// DefaultDRSInterval is DRS.Interval's fallback when unset.
	DefaultDRSInterval = 5 * time.Minute
	// DefaultDRSCooldown is DRS.Cooldown's fallback when unset.
	DefaultDRSCooldown = time.Hour
)

// DRS rebalances Machines across nodes by reserved CPU and memory (see
// scheduler.PlanRebalance). Recommend only logs, records Events and exports
// metrics; automatic also creates the MachineMigrations, one round at a
// time. Off by default.
type DRS struct {
	Mode                  string
	Interval              time.Duration
	ThresholdPercent      int
	MinImprovementPercent int
	MaxMoves              int
	// Strategy mirrors MachineMigrationSpec.Strategy: live|auto|cold.
	// With live (the default) only Machines live migration can move are
	// considered.
	Strategy string
	// Cooldown keeps a Machine the rebalancer moved in place this long.
	Cooldown time.Duration
}

// ValidateDRSMode rejects an unknown -drs-mode.
func ValidateDRSMode(mode string) error {
	switch mode {
	case "", DRSModeOff, DRSModeRecommend, DRSModeAutomatic:
		return nil
	}
	return fmt.Errorf("unknown DRS mode %q: want off|recommend|automatic", mode)
}

func (d DRS) interval() time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return DefaultDRSInterval
}

func (d DRS) cooldown() time.Duration {
	if d.Cooldown > 0 {
		return d.Cooldown
	}
	return DefaultDRSCooldown
}

func (d DRS) strategy() string {
	if d.Strategy == "" {
		return "live"
	}
	return d.Strategy
}

// RebalanceOptions is the planner configuration this DRS runs with; busy
// holds namespace/name of Machines already migrating.
func (d DRS) RebalanceOptions(busy map[string]bool, now time.Time) scheduler.RebalanceOptions {
	return scheduler.RebalanceOptions{
		ThresholdPercent:      d.ThresholdPercent,
		MinImprovementPercent: d.MinImprovementPercent,
		MaxMoves:              d.MaxMoves,
		LiveOnly:              d.strategy() == "live",
		Busy:                  busy,
		Cooldown:              d.cooldown(),
		Now:                   now,
	}
}

// drsPlanKey identifies a plan's moves, so recommend mode records Events
// only when the recommendation changes.
func drsPlanKey(plan scheduler.RebalancePlan) string {
	var b strings.Builder
	for _, mv := range plan.Moves {
		fmt.Fprintf(&b, "%s/%s:%s>%s;", mv.Namespace, mv.Machine, mv.From, mv.To)
	}
	return b.String()
}

// reconcileDRS runs one rebalancing round every Interval. A round is
// skipped while any migration the rebalancer started is still in flight,
// so automatic mode never stacks moves on top of unfinished ones.
func (c *Controller) reconcileDRS(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) {
	mode := c.DRS.Mode
	if mode == "" || mode == DRSModeOff {
		return
	}
	now := time.Now().UTC()
	if !c.drsLastRun.IsZero() && now.Sub(c.drsLastRun) < c.DRS.interval() {
		return
	}
	c.drsLastRun = now

	busy := map[string]bool{}
	drsInFlight := 0
	for _, mig := range migrations {
		if cordonEvacuateTerminal(mig.Status.Phase) {
			continue
		}
		busy[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		if mig.Metadata.Labels[model.LabelDRSMigration] == "true" {
			drsInFlight++
		}
	}
	plan := c.Scheduler.PlanRebalance(nodes, machines, c.DRS.RebalanceOptions(busy, now))
	if c.Metrics != nil {
		c.Metrics.ObserveDRSPlan(plan)
	}
	c.Log.Info("drs: plan", "mode", mode, "imbalanceBefore", plan.ImbalanceBefore, "imbalanceAfter", plan.ImbalanceAfter, "moves", len(plan.Moves), "reason", plan.Reason)
	if len(plan.Moves) == 0 {
		c.drsLastPlan = ""
		return
	}
	if mode == DRSModeRecommend {
		key := drsPlanKey(plan)
		if key == c.drsLastPlan {
			return
		}
		c.drsLastPlan = key
		for _, mv := range plan.Moves {
			m, ok := findMachine(machines, mv.Namespace, mv.Machine)
			if !ok {
				continue
			}
			msg := fmt.Sprintf("DRS recommends moving this Machine from %s to %s (cluster imbalance %d%% -> %d%%)", mv.From, mv.To, plan.ImbalanceBefore, mv.Imbalance)
			c.Log.Info("drs: recommendation", "namespace", mv.Namespace, "machine", mv.Machine, "from", mv.From, "to", mv.To)
			if err := c.Kube.RecordEvent(ctx, mv.Namespace, "Machine", mv.Machine, m.Metadata.UID, "DRSRecommendation", msg, "Normal"); err != nil {
				c.Log.Error("drs: record event failed", "namespace", mv.Namespace, "machine", mv.Machine, "error", err)
			}
			if c.Metrics != nil {
				c.Metrics.ObserveDRSMove("recommended")
			}
		}
		return
	}
	if drsInFlight > 0 {
		c.Log.Info("drs: previous round still migrating; not starting another", "inFlight", drsInFlight)
		return
	}
	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("drs: list machine disruption budgets failed", "error", err)
		return
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("drs: load budget states failed", "error", err)
		return
	}
	for _, mv := range plan.Moves {
		m, ok := findMachine(machines, mv.Namespace, mv.Machine)
		if !ok {
			continue
		}
		if blocker := AdmitDisruption(states, m); blocker != "" {
			c.Log.Info("drs: move blocked by disruption budget", "namespace", mv.Namespace, "machine", mv.Machine, "reason", blocker)
			if c.Metrics != nil {
				c.Metrics.ObserveDRSMove("blocked")
			}
			continue
		}
		migration := model.MachineMigration{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
			Metadata: model.ObjectMeta{
				Name:      "drs-" + mv.Machine + "-" + now.Format("20060102-150405"),
				Namespace: mv.Namespace,
				Labels:    map[string]string{model.LabelDRSMigration: "true"},
			},
			Spec: model.MachineMigrationSpec{MachineName: mv.Machine, Strategy: c.DRS.strategy(), TargetNode: mv.To},
		}
		if _, err := c.Kube.CreateMachineMigration(ctx, mv.Namespace, migration); err != nil {
			c.Log.Error("drs: create machine migration failed", "namespace", mv.Namespace, "machine", mv.Machine, "error", err)
			continue
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{model.AnnotationDRSMovedAt: now.Format(time.RFC3339)}}}
		if err := c.Kube.PatchMachine(ctx, mv.Namespace, mv.Machine, patch); err != nil {
			c.Log.Error("drs: patch moved-at annotation failed", "namespace", mv.Namespace, "machine", mv.Machine, "error", err)
		}
		msg := fmt.Sprintf("DRS is moving this Machine from %s to %s (cluster imbalance %d%% -> %d%%) with MachineMigration %s", mv.From, mv.To, plan.ImbalanceBefore, mv.Imbalance, migration.Metadata.Name)
		if err := c.Kube.RecordEvent(ctx, mv.Namespace, "Machine", mv.Machine, m.Metadata.UID, "DRSMigration", msg, "Normal"); err != nil {
			c.Log.Error("drs: record event failed", "namespace", mv.Namespace, "machine", mv.Machine, "error", err)
		}
		c.Log.Info("drs: created machine migration", "namespace", mv.Namespace, "machine", mv.Machine, "from", mv.From, "to", mv.To, "migration", migration.Metadata.Name)
		if c.Metrics != nil {
			c.Metrics.ObserveDRSMove("started")
		}
	}
}

func findMachine(machines []model.Machine, namespace, name string) (model.Machine, bool) {
	for _, m := range machines {
		if m.Namespace() == namespace && m.Metadata.Name == name {
			return m, true
		}
	}
	return model.Machine{}, false
}
