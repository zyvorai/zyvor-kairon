// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"testing"
	"time"
)

func TestSetConditionPreservesLastTransitionTimeWhenUnchanged(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	conds := []Condition{{
		Type: "Ready", Status: "True", Reason: "FluxVMReconciled",
		LastTransitionTime: t0,
	}}
	out := SetCondition(conds, Condition{Type: "Ready", Status: "True", Reason: "FluxVMReconciled"})
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	if !out[0].LastTransitionTime.Equal(t0) {
		t.Fatalf("LTT reset: got %v want %v", out[0].LastTransitionTime, t0)
	}
}

func TestSetConditionBumpsLastTransitionTimeOnTransition(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	conds := []Condition{{
		Type: "Ready", Status: "True", Reason: "FluxVMReconciled",
		LastTransitionTime: t0,
	}}
	before := time.Now().UTC()
	out := SetCondition(conds, Condition{Type: "Ready", Status: "False", Reason: "PoweredOff"})
	if out[0].LastTransitionTime.Equal(t0) {
		t.Fatal("expected LTT bump on Status change")
	}
	if out[0].LastTransitionTime.Before(before) {
		t.Fatalf("LTT %v before call start %v", out[0].LastTransitionTime, before)
	}
}

func TestSetConditionPreservesOtherTypes(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	conds := []Condition{
		{Type: ConditionNodeUnreachable, Status: "True", Reason: "NodeNotReadyOrMissing", LastTransitionTime: t0},
		{Type: "Ready", Status: "True", Reason: "FluxVMReconciled", LastTransitionTime: t0},
	}
	out := SetCondition(conds, Condition{Type: "Ready", Status: "True", Reason: "FluxVMReconciled"})
	if len(out) != 2 {
		t.Fatalf("wiped other conditions: %+v", out)
	}
	got, ok := FindCondition(out, ConditionNodeUnreachable)
	if !ok || got.Status != "True" {
		t.Fatalf("NodeUnreachable lost: %+v", out)
	}
}

func TestMachineStatusEqualIgnoringVolatile(t *testing.T) {
	base := MachineStatus{Phase: "Running", NodeName: "w1", RuntimeID: "vm-1", ObservedGeneration: 3}
	a := base
	a.ResourceUsage = &ResourceUsage{CPUPercent: 10, MemoryBytes: 100}
	b := base
	b.ResourceUsage = &ResourceUsage{CPUPercent: 99, MemoryBytes: 999}
	if !MachineStatusEqualIgnoringVolatile(a, b) {
		t.Fatal("expected equal when only ResourceUsage differs")
	}
	b.Phase = "Stopped"
	if MachineStatusEqualIgnoringVolatile(a, b) {
		t.Fatal("expected unequal when Phase differs")
	}
}
