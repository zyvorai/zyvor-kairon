// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import "testing"

func usageMachine(name, nodeName string, cpuPercent float64, memoryBytes uint64) Machine {
	return Machine{
		Metadata: ObjectMeta{Name: name, Namespace: "default"},
		Spec:     MachineSpec{NodeName: nodeName},
		Status: MachineStatus{
			ResourceUsage: &ResourceUsage{CPUPercent: cpuPercent, MemoryBytes: memoryBytes},
		},
	}
}

// TestAggregateUsageByNodeSortsAndGroupsByNode is this function's own
// pure-function unit test, kept alongside the type it groups/sums --
// internal/kaironctl's kaironctl_test.go re-exercises the same function
// through `kaironctl top nodes`' HTTP/flag-parsing layer, and
// internal/uiapi's nodeusage_test.go through GET /api/v1/nodes/usage;
// this one is the fastest, most direct check of the shared rule itself.
func TestAggregateUsageByNodeSortsAndGroupsByNode(t *testing.T) {
	got := AggregateUsageByNode([]Machine{
		usageMachine("vm-3", "worker-2", 10, 100),
		usageMachine("vm-1", "worker-1", 20, 200),
		usageMachine("vm-2", "worker-1", 5, 50),
	})
	want := []NodeUsageAggregate{
		{Node: "worker-1", Machines: 2, CPUPercent: 25, MemoryBytes: 250},
		{Node: "worker-2", Machines: 1, CPUPercent: 10, MemoryBytes: 100},
	}
	if len(got) != len(want) {
		t.Fatalf("AggregateUsageByNode() = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAggregateUsageByNodeUnscheduledMachineGroupsUnderDash mirrors
// `kaironctl get machines`' own dash(m.Spec.NodeName) rendering for a
// not-yet-scheduled Machine (empty Spec.NodeName) -- it must group under
// "-" rather than under "" or its own uniquely-empty bucket.
func TestAggregateUsageByNodeUnscheduledMachineGroupsUnderDash(t *testing.T) {
	got := AggregateUsageByNode([]Machine{
		usageMachine("vm-1", "", 10, 100),
	})
	if len(got) != 1 || got[0].Node != "-" {
		t.Fatalf("AggregateUsageByNode() = %+v, want a single row grouped under \"-\"", got)
	}
}

// TestAggregateUsageByNodeCountsUnreportedMachinesWithoutPanicking covers
// a Machine with no ResourceUsage at all sharing a node with one that has
// reported -- it must still count toward Machines and contribute zero to
// the sums, never panic on the nil *ResourceUsage.
func TestAggregateUsageByNodeCountsUnreportedMachinesWithoutPanicking(t *testing.T) {
	got := AggregateUsageByNode([]Machine{
		usageMachine("vm-1", "worker-1", 40, 1024*1024*1024),
		{Metadata: ObjectMeta{Name: "vm-2", Namespace: "default"}, Spec: MachineSpec{NodeName: "worker-1"}},
	})
	if len(got) != 1 {
		t.Fatalf("expected a single worker-1 row, got %+v", got)
	}
	if got[0].Machines != 2 {
		t.Errorf("expected both machines counted, got %+v", got[0])
	}
	if got[0].CPUPercent != 40 || got[0].MemoryBytes != 1024*1024*1024 {
		t.Errorf("expected sums to reflect only the reporting machine, got %+v", got[0])
	}
}
