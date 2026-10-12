// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

type recordedCall struct {
	method, path string
	body         map[string]any
}

// recordingKube is a fake apiserver that accepts every write and records
// it, serving only the MachineDisruptionBudget list.
type recordingKube struct {
	mu      sync.Mutex
	budgets []model.MachineDisruptionBudget
	calls   []recordedCall
}

func newRecordingController(t *testing.T, budgets []model.MachineDisruptionBudget) (*Controller, *recordingKube) {
	t.Helper()
	fake := &recordingKube{budgets: budgets}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets" {
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: fake.budgets})
			return
		}
		if r.Method == http.MethodGet {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		fake.calls = append(fake.calls, recordedCall{method: r.Method, path: r.URL.Path, body: body})
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Maintenance: NodeMaintenance{Enabled: true}}, fake
}

func (f *recordingKube) find(method, pathSuffix string) []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedCall
	for _, c := range f.calls {
		if c.method == method && strings.HasSuffix(c.path, pathSuffix) {
			out = append(out, c)
		}
	}
	return out
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func maintNode(name string, annotations map[string]string) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
	n.Metadata.Annotations = annotations
	n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
	n.Status.Allocatable = map[string]string{"cpu": "8", "memory": "16Gi"}
	return n
}

func maintMachine(name, node string) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: node, PowerState: "Running", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	m.Status.Phase = "Running"
	return m
}

func TestNodeMaintenanceCordonsAndMigrates(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	nodes := []model.Node{maintNode("a", map[string]string{model.AnnotationMaintenance: "true"}), maintNode("b", nil)}
	machines := []model.Machine{maintMachine("vm1", "a")}

	ctl.reconcileNodeMaintenance(context.Background(), machines, nodes, nil, scheduler.BuildNodeLoad(machines))

	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	if len(patches) < 2 || dig(patches[0].body, "spec", "unschedulable") != true || dig(patches[0].body, "metadata", "annotations", model.AnnotationMaintenanceCordoned) != "true" {
		t.Fatalf("node patches=%+v, want a cordon first", patches)
	}
	created := fake.find(http.MethodPost, "/namespaces/prod/machinemigrations")
	if len(created) != 1 || dig(created[0].body, "spec", "machineName") != "vm1" || dig(created[0].body, "spec", "strategy") != "live" {
		t.Fatalf("migrations=%+v, want one live migration of vm1", created)
	}
	last := patches[len(patches)-1]
	if dig(last.body, "metadata", "annotations", model.AnnotationMaintenanceState) != model.MaintenanceStateEntering {
		t.Fatalf("state patch=%+v", last.body)
	}
	if !nodes[0].Spec.Unschedulable {
		t.Fatal("the reconcile's node list should see the cordon")
	}
}

func TestNodeMaintenanceWaitFallbackLeavesMachineRunning(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	nodes := []model.Node{maintNode("a", map[string]string{model.AnnotationMaintenance: "true"})}
	machines := []model.Machine{maintMachine("vm1", "a")}

	ctl.reconcileNodeMaintenance(context.Background(), machines, nodes, nil, scheduler.BuildNodeLoad(machines))

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 0 {
		t.Fatalf("migrations=%+v, want none (no other node)", got)
	}
	if got := fake.find(http.MethodPatch, "/machines/vm1"); len(got) != 0 {
		t.Fatalf("machine patches=%+v, want none with fallback wait", got)
	}
	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	msg, _ := dig(patches[len(patches)-1].body, "metadata", "annotations", model.AnnotationMaintenanceMessage).(string)
	if !strings.Contains(msg, "1 with no eligible target node") {
		t.Fatalf("message=%q", msg)
	}
}

func TestNodeMaintenanceStopFallbackParksMachine(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	nodes := []model.Node{maintNode("a", map[string]string{model.AnnotationMaintenance: "true", model.AnnotationMaintenanceFallback: "hibernate"})}
	fc := maintMachine("fc", "a")
	fc.Spec.Runtime.Backend = "firecracker"
	machines := []model.Machine{maintMachine("vm1", "a"), fc}

	ctl.reconcileNodeMaintenance(context.Background(), machines, nodes, nil, scheduler.BuildNodeLoad(machines))

	for name, want := range map[string]string{"vm1": model.PowerStateHibernated, "fc": "Stopped"} {
		got := fake.find(http.MethodPatch, "/machines/"+name)
		if len(got) != 1 || dig(got[0].body, "spec", "powerState") != want {
			t.Fatalf("%s patches=%+v, want powerState %s", name, got, want)
		}
		raw, _ := dig(got[0].body, "metadata", "annotations", model.AnnotationMaintenanceParked).(string)
		var p model.MaintenanceParked
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Node != "a" || p.From != "Running" || p.To != want {
			t.Fatalf("%s parked=%q", name, raw)
		}
	}
}

func TestNodeMaintenanceReadyOnceEmpty(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	n := maintNode("a", map[string]string{model.AnnotationMaintenance: "true", model.AnnotationMaintenanceCordoned: "true"})
	n.Spec.Unschedulable = true
	stopped := maintMachine("vm1", "a")
	stopped.Spec.PowerState = "Stopped"
	stopped.Status.Phase = "Stopped"

	ctl.reconcileNodeMaintenance(context.Background(), []model.Machine{stopped}, []model.Node{n}, nil, map[string]scheduler.NodeLoad{})

	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	if len(patches) != 1 || dig(patches[0].body, "metadata", "annotations", model.AnnotationMaintenanceState) != model.MaintenanceStateReady {
		t.Fatalf("patches=%+v, want only the InMaintenance state (already cordoned)", patches)
	}
	if ev := fake.find(http.MethodPost, "/namespaces/default/events"); len(ev) != 1 {
		t.Fatalf("events=%+v, want MaintenanceReady", ev)
	}
}

func TestNodeMaintenanceStoppingMachineKeepsEntering(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	n := maintNode("a", map[string]string{model.AnnotationMaintenance: "true"})
	n.Spec.Unschedulable = true
	m := maintMachine("vm1", "a")
	m.Spec.PowerState = "Stopped"

	ctl.reconcileNodeMaintenance(context.Background(), []model.Machine{m}, []model.Node{n}, nil, map[string]scheduler.NodeLoad{})

	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	if len(patches) != 1 || dig(patches[0].body, "metadata", "annotations", model.AnnotationMaintenanceState) != model.MaintenanceStateEntering {
		t.Fatalf("patches=%+v", patches)
	}
}

func TestNodeMaintenanceHonorsMaxParallelAndBudgets(t *testing.T) {
	budget := model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "db"},
		Spec:     model.MachineDisruptionBudgetSpec{MaxUnavailable: "0", Selector: map[string]string{"app": "db"}},
	}
	ctl, fake := newRecordingController(t, []model.MachineDisruptionBudget{budget})
	ctl.Maintenance.MaxParallel = 1
	nodes := []model.Node{maintNode("a", map[string]string{model.AnnotationMaintenance: "true"}), maintNode("b", nil)}
	db := maintMachine("db", "a")
	db.Metadata.Labels = map[string]string{"app": "db"}
	machines := []model.Machine{db, maintMachine("vm1", "a"), maintMachine("vm2", "a")}

	ctl.reconcileNodeMaintenance(context.Background(), machines, nodes, nil, scheduler.BuildNodeLoad(machines))

	created := fake.find(http.MethodPost, "/machinemigrations")
	if len(created) != 1 || dig(created[0].body, "spec", "machineName") != "vm1" {
		t.Fatalf("migrations=%+v, want only vm1 (db blocked by budget, vm2 over max parallel)", created)
	}
	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	msg, _ := dig(patches[len(patches)-1].body, "metadata", "annotations", model.AnnotationMaintenanceMessage).(string)
	if !strings.Contains(msg, "1 migrating") || !strings.Contains(msg, "1 blocked") || !strings.Contains(msg, "1 waiting") {
		t.Fatalf("message=%q", msg)
	}
}

func TestNodeMaintenanceLiveDowngradesToColdForPassthrough(t *testing.T) {
	m := maintMachine("gpu", "a")
	m.Metadata.Annotations = map[string]string{model.AnnotationVFIOBDF: "0000:3b:00.0"}
	if got := maintenanceStrategyFor(m, "live"); got != "cold" {
		t.Fatalf("strategy=%q", got)
	}
	if got := maintenanceStrategyFor(maintMachine("vm", "a"), "live"); got != "live" {
		t.Fatalf("strategy=%q", got)
	}
}

func TestNodeMaintenanceInvalidAnnotationReportsAndDoesNothing(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	nodes := []model.Node{maintNode("a", map[string]string{model.AnnotationMaintenance: "true", model.AnnotationMaintenanceFallback: "delete"}), maintNode("b", nil)}
	machines := []model.Machine{maintMachine("vm1", "a")}

	ctl.reconcileNodeMaintenance(context.Background(), machines, nodes, nil, scheduler.BuildNodeLoad(machines))

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 0 {
		t.Fatalf("migrations=%+v", got)
	}
	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	msg, _ := dig(patches[len(patches)-1].body, "metadata", "annotations", model.AnnotationMaintenanceMessage).(string)
	if !strings.Contains(msg, "want wait|stop|hibernate") {
		t.Fatalf("message=%q", msg)
	}
}

func TestNodeMaintenanceExitUncordonsAndRestores(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	n := maintNode("a", map[string]string{model.AnnotationMaintenanceCordoned: "true", model.AnnotationMaintenanceState: model.MaintenanceStateReady})
	n.Spec.Unschedulable = true
	parked := maintMachine("vm1", "a")
	parked.Spec.PowerState = "Stopped"
	parked.Metadata.Annotations = map[string]string{model.AnnotationMaintenanceParked: model.MaintenanceParked{Node: "a", From: "Running", To: "Stopped"}.Encode()}
	changed := maintMachine("vm2", "a")
	changed.Spec.PowerState = "Halted"
	changed.Metadata.Annotations = map[string]string{model.AnnotationMaintenanceParked: model.MaintenanceParked{Node: "a", From: "Running", To: "Stopped"}.Encode()}

	ctl.reconcileNodeMaintenance(context.Background(), []model.Machine{parked, changed}, []model.Node{n}, nil, map[string]scheduler.NodeLoad{})

	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	if len(patches) != 1 || dig(patches[0].body, "spec", "unschedulable") != false {
		t.Fatalf("node patches=%+v, want an uncordon", patches)
	}
	if got := fake.find(http.MethodPatch, "/machines/vm1"); len(got) != 1 || dig(got[0].body, "spec", "powerState") != "Running" {
		t.Fatalf("vm1 patches=%+v, want powerState Running", got)
	}
	if got := fake.find(http.MethodPatch, "/machines/vm2"); len(got) != 1 || dig(got[0].body, "spec") != nil {
		t.Fatalf("vm2 patches=%+v, want only the annotation cleared (operator changed its power state)", got)
	}
}

func TestNodeMaintenanceExitLeavesOperatorCordon(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	n := maintNode("a", map[string]string{model.AnnotationMaintenanceState: model.MaintenanceStateReady})
	n.Spec.Unschedulable = true

	ctl.reconcileNodeMaintenance(context.Background(), nil, []model.Node{n}, nil, map[string]scheduler.NodeLoad{})

	patches := fake.find(http.MethodPatch, "/api/v1/nodes/a")
	if len(patches) != 1 || dig(patches[0].body, "spec") != nil {
		t.Fatalf("patches=%+v, want annotations cleared without uncordoning", patches)
	}
}

func TestCordonEvacuationSkipsMaintenanceNodes(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}
	n := cordonedNode("worker-1")
	n.Metadata.Annotations = map[string]string{model.AnnotationMaintenance: "true"}

	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{n}, nil)

	if fake.createdMigration != nil {
		t.Fatal("cordon evacuation should leave a maintenance node to NodeMaintenance")
	}
}
