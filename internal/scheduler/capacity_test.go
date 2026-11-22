// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package scheduler

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestChooseRejectsInsufficientCPU(t *testing.T) {
	n := node("a", true, true)
	n.Status.Allocatable = map[string]string{"cpu": "2", "memory": "8Gi"}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "big"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "4", Memory: "1Gi"}, PowerState: "Running"},
	}
	s := Scheduler{RequireCapableLabel: true}
	_, err := s.Choose(m, []model.Node{n}, nil, map[string]NodeLoad{}, "")
	if err == nil || !strings.Contains(err.Error(), "insufficient CPU") {
		t.Fatalf("err=%v", err)
	}
}

func TestChoosePrefersMoreRemainingCapacity(t *testing.T) {
	a := node("a", true, true)
	a.Status.Allocatable = map[string]string{"cpu": "8", "memory": "16Gi"}
	b := node("b", true, true)
	b.Status.Allocatable = map[string]string{"cpu": "8", "memory": "16Gi"}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}, PowerState: "Running"},
	}
	load := map[string]NodeLoad{
		"a": {Count: 1, CPUMilli: 6000, MemoryMiB: 12 * 1024},
		"b": {Count: 3, CPUMilli: 1000, MemoryMiB: 2 * 1024},
	}
	s := Scheduler{RequireCapableLabel: true}
	got, err := s.Choose(m, []model.Node{a, b}, nil, load, "")
	if err != nil || got != "b" {
		t.Fatalf("got %q err=%v (want b: more remaining capacity despite higher VM count)", got, err)
	}
}

func TestReservePreventsSameTickOvercommit(t *testing.T) {
	n := node("a", true, true)
	n.Status.Allocatable = map[string]string{"cpu": "4", "memory": "8Gi"}
	m1 := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"}, PowerState: "Running"},
	}
	m2 := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm2"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"}, PowerState: "Running"},
	}
	m3 := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm3"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "4Gi"}, PowerState: "Running"},
	}
	s := Scheduler{RequireCapableLabel: true}
	load := map[string]NodeLoad{}
	got, err := s.Choose(m1, []model.Node{n}, nil, load, "")
	if err != nil || got != "a" {
		t.Fatalf("first: %q %v", got, err)
	}
	Reserve(load, "a", m1)
	got, err = s.Choose(m2, []model.Node{n}, nil, load, "")
	if err != nil || got != "a" {
		t.Fatalf("second: %q %v", got, err)
	}
	Reserve(load, "a", m2)
	_, err = s.Choose(m3, []model.Node{n}, nil, load, "")
	if err == nil {
		t.Fatal("expected third placement to fail after reservations filled the node")
	}
}
