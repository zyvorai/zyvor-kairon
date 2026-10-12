// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

const (
	// DefaultMaintenanceMaxParallel is NodeMaintenance.MaxParallel's
	// fallback when unset.
	DefaultMaintenanceMaxParallel = 2
	// DefaultMaintenanceMinRetryInterval is
	// NodeMaintenance.MinRetryInterval's fallback when unset.
	DefaultMaintenanceMinRetryInterval = 2 * time.Minute
)

// NodeMaintenance drives node maintenance mode (model.AnnotationMaintenance).
// Unlike CordonEvacuation it acts only on nodes an operator explicitly put
// into maintenance, so it is on by default.
type NodeMaintenance struct {
	Enabled bool
	// MaxParallel caps in-flight maintenance migrations per node.
	MaxParallel int
	// MinRetryInterval throttles retries for a Machine whose migration was
	// blocked or ended without moving it.
	MinRetryInterval time.Duration
}

func (c NodeMaintenance) maxParallel() int {
	if c.MaxParallel > 0 {
		return c.MaxParallel
	}
	return DefaultMaintenanceMaxParallel
}

func (c NodeMaintenance) minRetryInterval() time.Duration {
	if c.MinRetryInterval > 0 {
		return c.MinRetryInterval
	}
	return DefaultMaintenanceMinRetryInterval
}

// maintenanceTally counts what keeps a node from being InMaintenance.
type maintenanceTally struct {
	migrating, waiting, noTarget, blocked, stopping int
	noTargetReason                                  string
}

func (t maintenanceTally) remaining() int {
	return t.migrating + t.waiting + t.noTarget + t.blocked + t.stopping
}

func (t maintenanceTally) message() string {
	if t.remaining() == 0 {
		return "no Machines running on this node"
	}
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(t.migrating, "migrating")
	add(t.stopping, "stopping for maintenance")
	add(t.blocked, "blocked by a MachineDisruptionBudget")
	add(t.waiting, "waiting to retry")
	add(t.noTarget, "with no eligible target node")
	msg := fmt.Sprintf("%d Machine(s) still on this node: %s", t.remaining(), strings.Join(parts, ", "))
	if t.noTargetReason != "" {
		msg += " (" + t.noTargetReason + ")"
	}
	return msg
}

// maintenanceStrategyFor downgrades live to cold for a Machine live
// migration refuses, so maintenance never parks a Machine cold migration
// could move.
func maintenanceStrategyFor(m model.Machine, strategy string) string {
	if strategy != "live" {
		return strategy
	}
	if !liveBackendEligible(m.Spec.Runtime.Backend) || len(m.Spec.Devices) > 0 || len(m.Spec.DeviceClaims) > 0 || m.Metadata.Annotations[model.AnnotationVFIOBDF] != "" {
		return "cold"
	}
	return "live"
}

// maintenanceParkState is the power state a fallback parks m in.
// Hibernation needs the qemu backend; anything else is stopped.
func maintenanceParkState(m model.Machine, fallback string) string {
	if fallback == model.MaintenanceFallbackHibernate && liveBackendEligible(m.Spec.Runtime.Backend) {
		return model.PowerStateHibernated
	}
	return "Stopped"
}

// reconcileNodeMaintenance enters and exits maintenance for every node.
// nodeLoad is updated as migrations are queued, so two Machines leaving
// the same node in one tick don't both count on the same free capacity.
func (c *Controller) reconcileNodeMaintenance(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration, nodeLoad map[string]scheduler.NodeLoad) {
	if !c.Maintenance.Enabled {
		return
	}
	var requested []string
	for _, n := range nodes {
		if !model.MaintenanceRequested(n) {
			c.exitNodeMaintenance(ctx, n, machines)
			continue
		}
		requested = append(requested, n.Metadata.Name)
	}
	if len(requested) == 0 {
		return
	}
	sort.Strings(requested)

	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("node maintenance: list machine disruption budgets failed", "error", err)
		return
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("node maintenance: load budget states failed", "error", err)
		return
	}
	inFlight := map[string]model.MachineMigration{}
	for _, mig := range migrations {
		if !cordonEvacuateTerminal(mig.Status.Phase) {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = mig
		}
	}
	// Every node in maintenance is cordoned before any target is chosen,
	// so two nodes draining together never pick each other.
	for i := range nodes {
		if model.MaintenanceRequested(nodes[i]) && !nodes[i].Spec.Unschedulable {
			if c.cordonForMaintenance(ctx, nodes[i]) {
				nodes[i].Spec.Unschedulable = true
			}
		}
	}
	for _, name := range requested {
		n, _ := nodeByName(nodes, name)
		c.drainForMaintenance(ctx, n, machines, nodes, inFlight, states, nodeLoad)
	}
}

func (c *Controller) cordonForMaintenance(ctx context.Context, n model.Node) bool {
	patch := map[string]any{
		"spec":     map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{model.AnnotationMaintenanceCordoned: "true"}},
	}
	if err := c.Kube.PatchNode(ctx, n.Metadata.Name, patch); err != nil {
		c.Log.Error("node maintenance: cordon failed", "node", n.Metadata.Name, "error", err)
		return false
	}
	c.Log.Info("node maintenance: cordoned node", "node", n.Metadata.Name)
	c.recordNodeEvent(ctx, n, "MaintenanceEntering", "kairon-controller cordoned the node for maintenance", "Normal")
	return true
}

func (c *Controller) drainForMaintenance(ctx context.Context, n model.Node, machines []model.Machine, nodes []model.Node, inFlight map[string]model.MachineMigration, states []*BudgetState, nodeLoad map[string]scheduler.NodeLoad) {
	node := n.Metadata.Name
	fallback, ferr := model.MaintenanceFallback(n)
	strategy, serr := model.MaintenanceStrategy(n)
	if ferr != nil || serr != nil {
		msg := "invalid maintenance annotations"
		for _, e := range []error{ferr, serr} {
			if e != nil {
				msg += ": " + e.Error()
			}
		}
		c.setMaintenanceState(ctx, n, model.MaintenanceStateEntering, msg, -1)
		return
	}
	now := time.Now().UTC()
	var tally maintenanceTally
	parallel := 0
	for _, mig := range inFlight {
		if strings.HasPrefix(mig.Metadata.Name, "maintenance-"+node+"-") {
			parallel++
		}
	}
	for _, m := range machines {
		if m.Spec.NodeName != node || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		key := m.Namespace() + "/" + m.Metadata.Name
		desired := m.DesiredPowerState()
		if model.PowerStateFreesCapacity(desired) {
			if m.Status.Phase != desired {
				tally.stopping++
			}
			continue
		}
		if _, ok := inFlight[key]; ok {
			tally.migrating++
			continue
		}
		if at, ok := m.Metadata.Annotations[model.AnnotationMaintenanceAttemptedAt]; ok {
			if t, err := time.Parse(time.RFC3339, at); err == nil && now.Sub(t) < c.Maintenance.minRetryInterval() {
				tally.waiting++
				continue
			}
		}
		mStrategy := maintenanceStrategyFor(m, strategy)
		target, terr := c.migrationTarget(ctx, m, "", mStrategy, nodes, machines, nodeLoad)
		if terr != nil {
			if fallback == model.MaintenanceFallbackWait {
				tally.noTarget++
				tally.noTargetReason = terr.Error()
				continue
			}
			if blocker := AdmitDisruption(states, m); blocker != "" {
				tally.blocked++
				continue
			}
			c.parkForMaintenance(ctx, m, node, desired, maintenanceParkState(m, fallback))
			tally.stopping++
			continue
		}
		if parallel >= c.Maintenance.maxParallel() {
			tally.waiting++
			continue
		}
		if blocker := AdmitDisruption(states, m); blocker != "" {
			c.Log.Info("node maintenance: blocked by disruption budget", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "reason", blocker)
			c.patchMaintenanceAttempt(ctx, m, now)
			tally.blocked++
			continue
		}
		migration := model.MachineMigration{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
			Metadata: model.ObjectMeta{Name: "maintenance-" + node + "-" + m.Metadata.Name + "-" + now.Format("20060102-150405"), Namespace: m.Namespace()},
			Spec:     model.MachineMigrationSpec{MachineName: m.Metadata.Name, Strategy: mStrategy},
		}
		if _, err := c.Kube.CreateMachineMigration(ctx, m.Namespace(), migration); err != nil {
			c.Log.Error("node maintenance: create machine migration failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			c.patchMaintenanceAttempt(ctx, m, now)
			tally.waiting++
			continue
		}
		c.Log.Info("node maintenance: created machine migration", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "target", target, "strategy", mStrategy, "migration", migration.Metadata.Name)
		c.patchMaintenanceAttempt(ctx, m, now)
		scheduler.Reserve(nodeLoad, target, m)
		parallel++
		tally.migrating++
	}
	state := model.MaintenanceStateEntering
	if tally.remaining() == 0 {
		state = model.MaintenanceStateReady
	}
	c.setMaintenanceState(ctx, n, state, tally.message(), tally.remaining())
}

func (c *Controller) parkForMaintenance(ctx context.Context, m model.Machine, node, from, to string) {
	parked := model.MaintenanceParked{Node: node, From: from, To: to}
	patch := map[string]any{
		"spec":     map[string]any{"powerState": to},
		"metadata": map[string]any{"annotations": map[string]any{model.AnnotationMaintenanceParked: parked.Encode()}},
	}
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		c.Log.Error("node maintenance: park machine failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		return
	}
	c.Log.Info("node maintenance: parked machine with no eligible target", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "powerState", to)
	if err := c.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, "MaintenanceParked",
		fmt.Sprintf("node %s entered maintenance and no other node can take this Machine; powerState %s -> %s until maintenance ends", node, from, to), "Normal"); err != nil {
		c.Log.Error("node maintenance: record event failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}

// exitNodeMaintenance undoes what maintenance did on a node that no longer
// requests it: uncordon if the controller cordoned it, restore each parked
// Machine whose power state is still the one maintenance set, and clear
// the status annotations.
func (c *Controller) exitNodeMaintenance(ctx context.Context, n model.Node, machines []model.Machine) {
	node := n.Metadata.Name
	for _, m := range machines {
		p, ok := model.ParseMaintenanceParked(m)
		if !ok || p.Node != node {
			continue
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationMaintenanceParked: nil}}}
		restored := m.DesiredPowerState() == p.To
		if restored {
			patch["spec"] = map[string]any{"powerState": p.From}
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
			c.Log.Error("node maintenance: restore machine failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			continue
		}
		if restored {
			c.Log.Info("node maintenance: restored machine power state", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "powerState", p.From)
		}
	}
	a := n.Metadata.Annotations
	_, hasState := a[model.AnnotationMaintenanceState]
	cordoned := a[model.AnnotationMaintenanceCordoned] == "true"
	if !hasState && !cordoned {
		return
	}
	annotations := map[string]any{
		model.AnnotationMaintenanceState:    nil,
		model.AnnotationMaintenanceMessage:  nil,
		model.AnnotationMaintenanceCordoned: nil,
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": annotations}}
	if cordoned {
		patch["spec"] = map[string]any{"unschedulable": false}
	}
	if err := c.Kube.PatchNode(ctx, node, patch); err != nil {
		c.Log.Error("node maintenance: exit failed", "node", node, "error", err)
		return
	}
	msg := "maintenance ended"
	if cordoned {
		msg += "; kairon-controller uncordoned the node"
	}
	c.Log.Info("node maintenance: exited", "node", node, "uncordoned", cordoned)
	c.recordNodeEvent(ctx, n, "MaintenanceExited", msg, "Normal")
	if c.Metrics != nil {
		c.Metrics.ObserveNodeMaintenance(node, "", 0)
	}
}

// setMaintenanceState patches the state/message annotations when they
// changed, and records an Event when the node reaches InMaintenance.
// remaining < 0 means the configuration is invalid.
func (c *Controller) setMaintenanceState(ctx context.Context, n model.Node, state, message string, remaining int) {
	if c.Metrics != nil {
		c.Metrics.ObserveNodeMaintenance(n.Metadata.Name, state, max(remaining, 0))
	}
	a := n.Metadata.Annotations
	if a[model.AnnotationMaintenanceState] == state && a[model.AnnotationMaintenanceMessage] == message {
		return
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		model.AnnotationMaintenanceState:   state,
		model.AnnotationMaintenanceMessage: message,
	}}}
	if err := c.Kube.PatchNode(ctx, n.Metadata.Name, patch); err != nil {
		c.Log.Error("node maintenance: patch state failed", "node", n.Metadata.Name, "error", err)
		return
	}
	c.Log.Info("node maintenance: state", "node", n.Metadata.Name, "state", state, "message", message)
	if state == model.MaintenanceStateReady && a[model.AnnotationMaintenanceState] != state {
		c.recordNodeEvent(ctx, n, "MaintenanceReady", "every Machine has left or been parked; the node is ready for maintenance", "Normal")
	}
	if remaining < 0 {
		c.recordNodeEvent(ctx, n, "MaintenanceInvalid", message, "Warning")
	}
}

func (c *Controller) patchMaintenanceAttempt(ctx context.Context, m model.Machine, at time.Time) {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		model.AnnotationMaintenanceAttemptedAt: at.Format(time.RFC3339),
	}}}
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		c.Log.Error("node maintenance: patch attempt annotation failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}

// recordNodeEvent posts an Event about a cluster-scoped Node, which lives
// in the default namespace the way kubectl and the kubelet record them.
func (c *Controller) recordNodeEvent(ctx context.Context, n model.Node, reason, message, eventType string) {
	if err := c.Kube.RecordEvent(ctx, "default", "Node", n.Metadata.Name, n.Metadata.UID, reason, message, eventType); err != nil {
		c.Log.Error("node event record failed", "node", n.Metadata.Name, "reason", reason, "error", err)
	}
}
