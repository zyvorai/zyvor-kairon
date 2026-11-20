// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func pinningMachine(name string, cpu string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "default"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: cpu, CPUPinning: true}},
	}
}

func TestChooseRejectsNodeWithNoPinnableCPUsLabel(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true) // no PinnableCPUsLabel set at all
	m := pinningMachine("vm", "2")
	_, err := s.Choose(m, []model.Node{n}, nil, LoadFromCounts(map[string]int{"a": 0}), "")
	if err == nil {
		t.Fatal("expected no eligible node when PinnableCPUsLabel is unset -- fail closed")
	}
}

func TestChooseRejectsNodeWithoutEnoughFreePinnableCPUs(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-3" // only 2 CPUs total
	m := pinningMachine("vm", "4")                     // requests more than the node has
	_, err := s.Choose(m, []model.Node{n}, nil, LoadFromCounts(map[string]int{"a": 0}), "")
	if err == nil {
		t.Fatal("expected no eligible node when the node has fewer pinnable CPUs than requested")
	}
}

func TestChooseAcceptsNodeWithEnoughFreePinnableCPUs(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-9" // 8 CPUs
	m := pinningMachine("vm", "4")
	got, err := s.Choose(m, []model.Node{n}, nil, LoadFromCounts(map[string]int{"a": 0}), "")
	if err != nil || got != "a" {
		t.Fatalf("got %q err=%v, want a", got, err)
	}
}

// TestChooseAccountsForAlreadyAllocatedCPUsOnOtherMachines is the real
// capacity-tracking regression guard: a node's pinnable set minus what
// sibling Machines already assigned to it have claimed via their own
// spec.resources.allocatedCpuSet, not just the raw label.
func TestChooseAccountsForAlreadyAllocatedCPUsOnOtherMachines(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-5" // 4 CPUs total
	existing := model.Machine{
		Metadata: model.ObjectMeta{Name: "already-here", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "a", Resources: model.ResourceSpec{AllocatedCPUSet: []uint32{2, 3}}},
	}
	m := pinningMachine("vm", "3") // only 2 free (4, 5) remain -- not enough
	_, err := s.Choose(m, []model.Node{n}, []model.Machine{existing}, LoadFromCounts(map[string]int{"a": 1}), "")
	if err == nil {
		t.Fatal("expected no eligible node once sibling allocations are accounted for")
	}
}

// TestChooseIgnoresAllocatedCPUsOnMachinesAssignedToOtherNodes proves the
// per-node accounting is actually per-node, not global.
func TestChooseIgnoresAllocatedCPUsOnMachinesAssignedToOtherNodes(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	a := node("a", true, true)
	a.Metadata.Labels[model.PinnableCPUsLabel] = "2-5"
	elsewhere := model.Machine{
		Metadata: model.ObjectMeta{Name: "on-other-node", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "b", Resources: model.ResourceSpec{AllocatedCPUSet: []uint32{2, 3, 4, 5}}},
	}
	m := pinningMachine("vm", "4")
	got, err := s.Choose(m, []model.Node{a}, []model.Machine{elsewhere}, LoadFromCounts(map[string]int{"a": 0}), "")
	if err != nil || got != "a" {
		t.Fatalf("got %q err=%v, want a (node b's allocations shouldn't affect node a)", got, err)
	}
}

// TestChooseNonPinningMachineIgnoresPinnableCPUsEntirely proves a
// Machine that never opted into cpuPinning schedules exactly as before --
// no behavior change for the overwhelmingly common case.
func TestChooseNonPinningMachineIgnoresPinnableCPUsEntirely(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true) // no PinnableCPUsLabel at all
	m := model.Machine{Metadata: model.ObjectMeta{Name: "plain", Namespace: "default"}}
	got, err := s.Choose(m, []model.Node{n}, nil, LoadFromCounts(map[string]int{"a": 0}), "")
	if err != nil || got != "a" {
		t.Fatalf("got %q err=%v, want a", got, err)
	}
}

func TestAllocateCPUSetPicksFirstFitAscending(t *testing.T) {
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-9"
	m := pinningMachine("vm", "3")
	got, err := AllocateCPUSet(m, n, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAllocateCPUSetSkipsAlreadyClaimedCPUs(t *testing.T) {
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-6"
	existing := model.Machine{
		Metadata: model.ObjectMeta{Name: "already-here", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "a", Resources: model.ResourceSpec{AllocatedCPUSet: []uint32{2, 3}}},
	}
	m := pinningMachine("vm", "2")
	got, err := AllocateCPUSet(m, n, []model.Machine{existing})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAllocateCPUSetErrorsWhenNotEnoughFreeCPUs(t *testing.T) {
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-3"
	m := pinningMachine("vm", "4")
	if _, err := AllocateCPUSet(m, n, nil); err == nil {
		t.Fatal("expected an error when the node doesn't have enough free CPUs")
	}
}

func TestAllocateCPUSetIgnoresDeletingSiblingMachines(t *testing.T) {
	n := node("a", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "2-5"
	now := time.Now()
	deleting := model.Machine{
		Metadata: model.ObjectMeta{Name: "going-away", Namespace: "default", DeletionTimestamp: &now},
		Spec:     model.MachineSpec{NodeName: "a", Resources: model.ResourceSpec{AllocatedCPUSet: []uint32{2, 3, 4, 5}}},
	}
	m := pinningMachine("vm", "2")
	got, err := AllocateCPUSet(m, n, []model.Machine{deleting})
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{2, 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v -- expected a deleting Machine's own allocation to be freed for reuse", got, want)
	}
}
