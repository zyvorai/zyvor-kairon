// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func rebalanceNodeFixture(name string) model.Node {
	n := node(name, true, true)
	n.Status.Allocatable = map[string]string{"cpu": "8", "memory": "16Gi"}
	return n
}

func runningOn(name, nodeName, cpu, mem string) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: nodeName, PowerState: "Running", Resources: model.ResourceSpec{CPU: cpu, Memory: mem}},
	}
	m.Status.Phase = "Running"
	return m
}

func TestPlanRebalanceMovesFromHotToCold(t *testing.T) {
	nodes := []model.Node{rebalanceNodeFixture("a"), rebalanceNodeFixture("b")}
	machines := []model.Machine{
		runningOn("vm1", "a", "2", "4Gi"),
		runningOn("vm2", "a", "2", "4Gi"),
		runningOn("vm3", "a", "2", "4Gi"),
	}
	plan := Scheduler{RequireCapableLabel: true}.PlanRebalance(nodes, machines, RebalanceOptions{MaxMoves: 3})
	if plan.ImbalanceBefore != 75 {
		t.Fatalf("ImbalanceBefore=%d, want 75 (a at 6/8 CPU, b empty)", plan.ImbalanceBefore)
	}
	if len(plan.Moves) != 1 || plan.Moves[0].From != "a" || plan.Moves[0].To != "b" {
		t.Fatalf("moves=%+v, want one a->b move", plan.Moves)
	}
	if plan.ImbalanceAfter != 25 {
		t.Fatalf("ImbalanceAfter=%d, want 25 (a 50%%, b 25%%): a second move would only swap which node is hotter", plan.ImbalanceAfter)
	}
}

func TestPlanRebalanceBelowThresholdPlansNothing(t *testing.T) {
	nodes := []model.Node{rebalanceNodeFixture("a"), rebalanceNodeFixture("b")}
	machines := []model.Machine{runningOn("vm1", "a", "1", "1Gi")}
	plan := Scheduler{}.PlanRebalance(nodes, machines, RebalanceOptions{})
	if len(plan.Moves) != 0 || !strings.Contains(plan.Reason, "below the 20% threshold") {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanRebalanceNeedsTwoNodes(t *testing.T) {
	plan := Scheduler{}.PlanRebalance([]model.Node{rebalanceNodeFixture("a")}, []model.Machine{runningOn("vm1", "a", "8", "16Gi")}, RebalanceOptions{})
	if len(plan.Moves) != 0 || !strings.Contains(plan.Reason, "at least 2") {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanRebalanceSkipsIneligibleNodesAndMachines(t *testing.T) {
	cordoned := rebalanceNodeFixture("c")
	cordoned.Spec.Unschedulable = true
	maint := rebalanceNodeFixture("d")
	maint.Metadata.Annotations = map[string]string{model.AnnotationMaintenance: "true"}
	nodes := []model.Node{rebalanceNodeFixture("a"), rebalanceNodeFixture("b"), cordoned, maint}

	pinned := runningOn("pinned", "a", "2", "4Gi")
	pinned.Spec.Resources.CPUPinning = true
	optedOut := runningOn("opted-out", "a", "2", "4Gi")
	optedOut.Metadata.Annotations = map[string]string{model.AnnotationDRS: "disabled"}
	passthrough := runningOn("gpu", "a", "2", "4Gi")
	passthrough.Metadata.Annotations = map[string]string{model.AnnotationVFIOBDF: "0000:3b:00.0"}
	busy := runningOn("busy", "a", "1", "1Gi")
	cooled := runningOn("cooled", "a", "1", "1Gi")
	cooled.Metadata.Annotations = map[string]string{model.AnnotationDRSMovedAt: time.Now().UTC().Format(time.RFC3339)}
	firecracker := runningOn("fc", "a", "1", "1Gi")
	firecracker.Spec.Runtime.Backend = "firecracker"

	plan := Scheduler{}.PlanRebalance(nodes, []model.Machine{pinned, optedOut, passthrough, busy, cooled, firecracker}, RebalanceOptions{
		MaxMoves: 5, LiveOnly: true, Busy: map[string]bool{"prod/busy": true}, Cooldown: time.Hour, Now: time.Now().UTC(),
	})
	if len(plan.Nodes) != 2 {
		t.Fatalf("nodes=%+v, want only a and b (cordoned and maintenance nodes excluded)", plan.Nodes)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("moves=%+v, want none: every Machine is excluded", plan.Moves)
	}
}

func TestPlanRebalanceHonorsRequiredAntiAffinity(t *testing.T) {
	a, b := rebalanceNodeFixture("a"), rebalanceNodeFixture("b")
	a.Metadata.Labels["zone"] = "z1"
	b.Metadata.Labels["zone"] = "z2"
	web1 := runningOn("web1", "a", "3", "4Gi")
	web1.Metadata.Labels = map[string]string{"app": "web"}
	web1.Spec.Placement.AntiAffinity = []model.MachineAffinityTerm{{LabelSelector: map[string]string{"app": "web"}, TopologyKey: "zone"}}
	web2 := runningOn("web2", "b", "1", "1Gi")
	web2.Metadata.Labels = map[string]string{"app": "web"}
	other := runningOn("other", "a", "3", "4Gi")

	plan := Scheduler{}.PlanRebalance([]model.Node{a, b}, []model.Machine{web1, web2, other}, RebalanceOptions{})
	if len(plan.Moves) != 1 || plan.Moves[0].Machine != "other" {
		t.Fatalf("moves=%+v, want only 'other' moved (web1 may not share zone z2 with web2)", plan.Moves)
	}
}

func TestPlanRebalanceRespectsTargetCapacity(t *testing.T) {
	a, b := rebalanceNodeFixture("a"), rebalanceNodeFixture("b")
	b.Status.Allocatable = map[string]string{"cpu": "2", "memory": "2Gi"}
	machines := []model.Machine{runningOn("big", "a", "4", "8Gi"), runningOn("big2", "a", "4", "8Gi")}
	plan := Scheduler{}.PlanRebalance([]model.Node{a, b}, machines, RebalanceOptions{})
	if len(plan.Moves) != 0 {
		t.Fatalf("moves=%+v, want none: nothing fits on b", plan.Moves)
	}
}

func TestPlanRebalanceDoesNotMutateBusy(t *testing.T) {
	nodes := []model.Node{rebalanceNodeFixture("a"), rebalanceNodeFixture("b")}
	machines := []model.Machine{runningOn("vm1", "a", "4", "4Gi"), runningOn("vm2", "a", "4", "4Gi")}
	busy := map[string]bool{}
	plan := Scheduler{}.PlanRebalance(nodes, machines, RebalanceOptions{Busy: busy})
	if len(plan.Moves) != 1 || len(busy) != 0 {
		t.Fatalf("moves=%+v busy=%v", plan.Moves, busy)
	}
}
