// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func pressuredNode(name string, host model.PSI, at time.Time, machines map[string]model.PSI) model.Node {
	n := rebalanceNodeFixture(name)
	if n.Metadata.Annotations == nil {
		n.Metadata.Annotations = map[string]string{}
	}
	n.Metadata.Annotations[model.AnnotationNodePressure] = model.NodePressure{
		SampledAt: at.UTC().Format(time.RFC3339), Host: host, Machines: machines,
	}.Encode()
	return n
}

func pressureOpts(now time.Time) RebalanceOptions {
	return RebalanceOptions{Signal: RebalanceSignalPressure, Now: now, MaxMoves: 3}
}

func TestPlanPressureMovesTheMostStalledMachineToTheCalmestNode(t *testing.T) {
	now := time.Now()
	nodes := []model.Node{
		pressuredNode("a", model.PSI{CPU: 40, IO: 5}, now, map[string]model.PSI{"prod/quiet": {CPU: 1}, "prod/stalled": {CPU: 30}}),
		pressuredNode("b", model.PSI{CPU: 8}, now, nil),
		pressuredNode("c", model.PSI{CPU: 2}, now, nil),
	}
	machines := []model.Machine{runningOn("quiet", "a", "1", "1Gi"), runningOn("stalled", "a", "1", "1Gi")}
	plan := Scheduler{}.PlanRebalance(nodes, machines, pressureOpts(now))
	if plan.Signal != RebalanceSignalPressure || plan.ImbalanceBefore != 38 {
		t.Fatalf("plan=%+v, want pressure signal with spread 38", plan)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("moves=%+v, want exactly one: a is the only hot node", plan.Moves)
	}
	mv := plan.Moves[0]
	if mv.Machine != "stalled" || mv.From != "a" || mv.To != "c" {
		t.Fatalf("move=%+v, want stalled a->c", mv)
	}
	if !strings.Contains(mv.Reason, "cpu pressure on a is 40.0%") || !strings.Contains(mv.Reason, "stalls 30.0%") {
		t.Fatalf("reason=%q", mv.Reason)
	}
	for _, u := range plan.Nodes {
		if u.Pressure == nil {
			t.Fatalf("node %s has no pressure in the plan", u.Node)
		}
	}
}

func TestPlanPressureBelowThresholdPlansNothing(t *testing.T) {
	now := time.Now()
	nodes := []model.Node{pressuredNode("a", model.PSI{IO: 24.9}, now, nil), pressuredNode("b", model.PSI{}, now, nil)}
	plan := Scheduler{}.PlanRebalance(nodes, []model.Machine{runningOn("vm", "a", "1", "1Gi")}, pressureOpts(now))
	if len(plan.Moves) != 0 || !strings.Contains(plan.Reason, "reaches the 25% threshold") {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanPressureNeedsACalmTarget(t *testing.T) {
	now := time.Now()
	nodes := []model.Node{pressuredNode("a", model.PSI{CPU: 60}, now, nil), pressuredNode("b", model.PSI{Memory: 13}, now, nil)}
	plan := Scheduler{}.PlanRebalance(nodes, []model.Machine{runningOn("vm", "a", "1", "1Gi")}, pressureOpts(now))
	if len(plan.Moves) != 0 || !strings.Contains(plan.Reason, "below 12.5%") {
		t.Fatalf("plan=%+v, want no move: b is not below half the threshold", plan)
	}
}

func TestPlanPressureIgnoresStaleAndMissingSamples(t *testing.T) {
	now := time.Now()
	nodes := []model.Node{
		pressuredNode("a", model.PSI{CPU: 60}, now, nil),
		pressuredNode("stale", model.PSI{}, now.Add(-10*time.Minute), nil),
		rebalanceNodeFixture("missing"),
	}
	plan := Scheduler{}.PlanRebalance(nodes, []model.Machine{runningOn("vm", "a", "1", "1Gi")}, pressureOpts(now))
	if len(plan.Moves) != 0 || !strings.Contains(plan.Reason, "1 node(s) with a pressure sample") ||
		!strings.Contains(plan.Reason, "missing, stale") {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanPressureKeepsHardFiltersAndOneMovePerTarget(t *testing.T) {
	now := time.Now()
	full := pressuredNode("full", model.PSI{}, now, nil)
	full.Status.Allocatable = map[string]string{"cpu": "2", "memory": "2Gi"}
	nodes := []model.Node{
		pressuredNode("a", model.PSI{CPU: 50}, now, nil),
		pressuredNode("b", model.PSI{IO: 30}, now, nil),
		pressuredNode("c", model.PSI{CPU: 1}, now, nil),
		full,
	}
	optedOut := runningOn("opted-out", "a", "1", "1Gi")
	optedOut.Metadata.Annotations = map[string]string{model.AnnotationDRS: "disabled"}
	machines := []model.Machine{optedOut, runningOn("big", "a", "4", "8Gi"), runningOn("small", "b", "1", "1Gi")}
	plan := Scheduler{}.PlanRebalance(nodes, machines, pressureOpts(now))
	if len(plan.Moves) != 2 {
		t.Fatalf("moves=%+v, want two", plan.Moves)
	}
	if mv := plan.Moves[0]; mv.Machine != "big" || mv.To != "c" {
		t.Fatalf("first move=%+v, want big a->c (full cannot fit it; opted-out stays)", mv)
	}
	if mv := plan.Moves[1]; mv.Machine != "small" || mv.To != "full" {
		t.Fatalf("second move=%+v, want small b->full: c already took a Machine this round", mv)
	}
}
