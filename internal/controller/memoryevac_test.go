// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func pressuredNode(name string, since time.Time) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Status.Conditions = []model.NodeCondition{
		{Type: "Ready", Status: "True"},
		{Type: model.NodeConditionKaironMemoryPressure, Status: "True", LastTransitionTime: since.UTC().Format(time.RFC3339)},
	}
	return n
}

func runningOn(node, name, mem string, priority int32) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: node, PowerState: "Running", Priority: priority},
	}
	m.Spec.Resources.Memory = mem
	m.Status.Phase = "Running"
	return m
}

func TestMemoryEvacuationMigratesAfterGrace(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.MemoryEvacuation = MemoryPressureEvacuation{Enabled: true}
	node := pressuredNode("worker-1", time.Now().Add(-5*time.Minute))

	ctl.reconcileMemoryPressureEvacuation(context.Background(), []model.Machine{runningOn("worker-1", "vm-1", "4Gi", 0)}, []model.Node{node}, nil)

	if fake.createdMigration == nil {
		t.Fatal("expected a MachineMigration")
	}
	if fake.createdMigration.Spec.MachineName != "vm-1" || fake.createdMigration.Spec.Strategy != "live" {
		t.Fatalf("migration spec %+v", fake.createdMigration.Spec)
	}
	if fake.patchedAnnos[model.AnnotationMemoryEvacuateAttemptedAt] == "" {
		t.Fatal("expected the memory-evacuate-attempted-at annotation")
	}
}

func TestMemoryEvacuationWaitsForGrace(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.MemoryEvacuation = MemoryPressureEvacuation{Enabled: true}
	node := pressuredNode("worker-1", time.Now().Add(-30*time.Second))

	ctl.reconcileMemoryPressureEvacuation(context.Background(), []model.Machine{runningOn("worker-1", "vm-1", "4Gi", 0)}, []model.Node{node}, nil)

	if fake.createdMigration != nil {
		t.Fatal("migrated before the grace period")
	}
}

func TestMemoryEvacuationOneMigrationPerNode(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.MemoryEvacuation = MemoryPressureEvacuation{Enabled: true}
	node := pressuredNode("worker-1", time.Now().Add(-5*time.Minute))
	inFlight := model.MachineMigration{Metadata: model.ObjectMeta{Name: "m", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "other"}}
	inFlight.Status.Phase = "Running"

	ctl.reconcileMemoryPressureEvacuation(context.Background(),
		[]model.Machine{runningOn("worker-1", "vm-1", "4Gi", 0), runningOn("worker-1", "other", "2Gi", 0)},
		[]model.Node{node}, []model.MachineMigration{inFlight})

	if fake.createdMigration != nil {
		t.Fatal("started a second migration off a node with one in flight")
	}
}

func TestMemoryEvacuationDisabled(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	node := pressuredNode("worker-1", time.Now().Add(-time.Hour))
	ctl.reconcileMemoryPressureEvacuation(context.Background(), []model.Machine{runningOn("worker-1", "vm-1", "4Gi", 0)}, []model.Node{node}, nil)
	if fake.createdMigration != nil {
		t.Fatal("migrated while disabled")
	}
}

func TestMemoryEvacuationCandidateOrder(t *testing.T) {
	gpu := runningOn("n", "gpu", "64Gi", 0)
	gpu.Spec.Devices = []model.MachineDevice{{Name: "g", Class: model.DeviceClassGPU}}
	stopped := runningOn("n", "stopped", "64Gi", 0)
	stopped.Status.Phase = "Stopped"
	ms := []model.Machine{
		runningOn("n", "small-low", "1Gi", 0),
		runningOn("n", "big-low", "8Gi", 0),
		runningOn("n", "big-high", "16Gi", 100),
		runningOn("other", "elsewhere", "32Gi", 0),
		gpu,
		stopped,
	}

	got := memoryEvacuationCandidates(ms, "n", true)
	names := make([]string, 0, len(got))
	for _, m := range got {
		names = append(names, m.Metadata.Name)
	}
	want := []string{"big-low", "small-low", "big-high"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
	if got := memoryEvacuationCandidates(ms, "n", false); len(got) != 4 || got[0].Metadata.Name != "gpu" {
		t.Fatalf("cold strategy keeps device Machines: %v", len(got))
	}
}

func TestMemoryPressureSince(t *testing.T) {
	now := time.Now()
	n := pressuredNode("a", now.Add(-time.Minute))
	if since, ok := memoryPressureSince(n, now); !ok || now.Sub(since) < 59*time.Second {
		t.Fatalf("since=%v ok=%v", since, ok)
	}
	n.Status.Conditions[1].Status = "False"
	if _, ok := memoryPressureSince(n, now); ok {
		t.Fatal("False condition counted as pressure")
	}
	n.Status.Conditions[1] = model.NodeCondition{Type: model.NodeConditionKaironMemoryPressure, Status: "True"}
	if since, ok := memoryPressureSince(n, now); !ok || !since.Equal(now) {
		t.Fatal("missing transition time must count as just turned True")
	}
}
