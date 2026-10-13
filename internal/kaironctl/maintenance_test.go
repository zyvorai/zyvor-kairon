// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

func TestMaintenanceEnterPatch(t *testing.T) {
	patch, err := maintenanceEnterPatch("stop", "")
	if err != nil {
		t.Fatal(err)
	}
	a := patch["metadata"].(map[string]any)["annotations"].(map[string]any)
	if a[model.AnnotationMaintenance] != "true" || a[model.AnnotationMaintenanceFallback] != "stop" || a[model.AnnotationMaintenanceStrategy] != nil {
		t.Fatalf("annotations=%v", a)
	}
	if _, err := maintenanceEnterPatch("delete", ""); err == nil {
		t.Fatal("want an error for an unknown fallback")
	}
	if _, err := maintenanceEnterPatch("", "teleport"); err == nil {
		t.Fatal("want an error for an unknown strategy")
	}
}

func TestMaintenanceExitPatchClearsOperatorAnnotations(t *testing.T) {
	a := maintenanceExitPatch()["metadata"].(map[string]any)["annotations"].(map[string]any)
	for _, k := range []string{model.AnnotationMaintenance, model.AnnotationMaintenanceFallback, model.AnnotationMaintenanceStrategy} {
		if v, ok := a[k]; !ok || v != nil {
			t.Fatalf("%s=%v", k, v)
		}
	}
}

func TestPrintMaintenanceStatus(t *testing.T) {
	var n model.Node
	n.Metadata.Name = "worker-2"
	n.Spec.Unschedulable = true
	n.Metadata.Annotations = map[string]string{
		model.AnnotationMaintenance:        "true",
		model.AnnotationMaintenanceState:   model.MaintenanceStateEntering,
		model.AnnotationMaintenanceMessage: "1 Machine(s) still on this node: 1 migrating",
	}
	var buf bytes.Buffer
	printMaintenanceStatus(&buf, []model.Node{n})
	out := buf.String()
	if !strings.Contains(out, "worker-2") || !strings.Contains(out, "Entering") || !strings.Contains(out, "1 migrating") || !strings.Contains(out, "false") {
		t.Fatalf("out=%q", out)
	}
}

type fakeDRSLister struct {
	machines   []model.Machine
	nodes      []model.Node
	migrations []model.MachineMigration
}

func (f fakeDRSLister) ListMachines(context.Context) ([]model.Machine, error) { return f.machines, nil }
func (f fakeDRSLister) ListNodes(context.Context) ([]model.Node, error)       { return f.nodes, nil }
func (f fakeDRSLister) ListMachineMigrations(context.Context) ([]model.MachineMigration, error) {
	return f.migrations, nil
}

func TestDRSPlanSkipsMigratingMachines(t *testing.T) {
	mkNode := func(name string) model.Node {
		var n model.Node
		n.Metadata.Name = name
		n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
		n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
		n.Status.Allocatable = map[string]string{"cpu": "8", "memory": "16Gi"}
		return n
	}
	mk := func(name string) model.Machine {
		m := model.Machine{Metadata: model.ObjectMeta{Name: name, Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "a", PowerState: "Running", Resources: model.ResourceSpec{CPU: "4", Memory: "4Gi"}}}
		m.Status.Phase = "Running"
		return m
	}
	f := fakeDRSLister{
		nodes:      []model.Node{mkNode("a"), mkNode("b")},
		machines:   []model.Machine{mk("vm1"), mk("vm2")},
		migrations: []model.MachineMigration{{Metadata: model.ObjectMeta{Namespace: "prod", Name: "m"}, Spec: model.MachineMigrationSpec{MachineName: "vm1"}, Status: model.MachineMigrationStatus{Phase: "Running"}}},
	}
	p, err := drsPlan(context.Background(), f, controller.DRS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Moves) != 1 || p.Moves[0].Machine != "vm2" {
		t.Fatalf("moves=%+v, want vm2 (vm1 is already migrating)", p.Moves)
	}
	var buf bytes.Buffer
	printDRSPlan(&buf, p)
	if !strings.Contains(buf.String(), "prod/vm2") || !strings.Contains(buf.String(), "imbalance: 100%") {
		t.Fatalf("out=%q", buf.String())
	}
}

func TestPrintDRSPlanNoMoves(t *testing.T) {
	var buf bytes.Buffer
	printDRSPlan(&buf, scheduler.RebalancePlan{Reason: "1 eligible node(s); rebalancing needs at least 2"})
	if !strings.Contains(buf.String(), "no moves: 1 eligible node(s)") {
		t.Fatalf("out=%q", buf.String())
	}
}

func TestPrintDRSPressurePlan(t *testing.T) {
	var buf bytes.Buffer
	printDRSPlan(&buf, scheduler.RebalancePlan{
		Signal:          scheduler.RebalanceSignalPressure,
		ImbalanceBefore: 37,
		Nodes: []scheduler.NodeUtilization{
			{Node: "a", Percent: 50, Machines: 2, Pressure: &model.PSI{CPU: 40.2, IO: 3}},
			{Node: "b", Percent: 10, Machines: 0},
		},
		Moves: []scheduler.RebalanceMove{{Namespace: "prod", Machine: "vm2", From: "a", To: "c", Reason: "cpu pressure on a is 40.2% (threshold 25%)"}},
	})
	out := buf.String()
	for _, want := range []string{"CPU PSI%", "40.2", "pressure spread: 37 points", "prod/vm2", "cpu pressure on a is 40.2%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("out=%q, missing %q", out, want)
		}
	}
	if !regexp.MustCompile(`(?m)^b\s+-\s+-\s+-\s+10\s+0$`).MatchString(out) {
		t.Fatalf("out=%q: an unmeasured node shows - for its PSI", out)
	}
}
