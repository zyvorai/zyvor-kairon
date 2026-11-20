// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// TestCountAssignedCountsPausedAlongsideRunning locks in a real behavior
// change: a Paused Machine keeps its FluxVM runtime (and the host memory
// backing it) alive, unlike Stopped, which tears the runtime down
// entirely -- so scheduler bin-packing must keep counting it against the
// node's capacity, the same as a Running one.
func TestCountAssignedCountsPausedAlongsideRunning(t *testing.T) {
	machines := []model.Machine{
		{Metadata: model.ObjectMeta{Name: "running"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Running"}},
		{Metadata: model.ObjectMeta{Name: "paused"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Paused"}},
		{Metadata: model.ObjectMeta{Name: "stopped"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Stopped"}},
		{Metadata: model.ObjectMeta{Name: "halted"}, Spec: model.MachineSpec{NodeName: "worker-1", PowerState: "Halted"}},
		{Metadata: model.ObjectMeta{Name: "unscheduled"}, Spec: model.MachineSpec{PowerState: "Running"}},
	}
	assigned := countAssigned(machines)
	if assigned["worker-1"] != 2 {
		t.Fatalf("expected Running+Paused to count (2), Stopped/Halted/unscheduled not to, got %+v", assigned)
	}
}

func TestCountAssignedIgnoresDeletedMachines(t *testing.T) {
	now := time.Now()
	deleting := model.Machine{
		Metadata: model.ObjectMeta{Name: "going-away", DeletionTimestamp: &now},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Paused"},
	}
	assigned := countAssigned([]model.Machine{deleting})
	if assigned["worker-1"] != 0 {
		t.Fatalf("expected a Machine being deleted not to count even if Paused, got %+v", assigned)
	}
}
