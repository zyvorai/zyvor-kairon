// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func intPtr(n int) *int { return &n }

func scheduledMachine(name, node string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: node, Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
}

func unscheduledMachine(name string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
}

func TestAdmitQuotaBlocksOnceMaxMachinesReached(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(2)},
	}
	machines := []model.Machine{scheduledMachine("a", "worker-1"), scheduledMachine("b", "worker-1")}
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{quota}, machines)
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("c")); blocker == "" {
		t.Fatal("expected the third machine to be blocked once maxMachines=2 is already reached")
	}
}

func TestAdmitQuotaAllowsUnderLimitAndSpendsAcrossPass(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(2)},
	}
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{quota}, nil)
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("a")); blocker != "" {
		t.Fatalf("expected first machine admitted, got %q", blocker)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("b")); blocker != "" {
		t.Fatalf("expected second machine admitted, got %q", blocker)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("c")); blocker == "" {
		t.Fatal("expected the third machine to be blocked -- the first two already spent the quota within this pass")
	}
}

func TestAdmitQuotaEnforcesTotalCPUAndMemory(t *testing.T) {
	cpuQuota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "cpu-q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxTotalCPU: "2"},
	}
	memQuota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "mem-q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxTotalMemory: "4Gi"},
	}
	existing := []model.Machine{scheduledMachine("a", "worker-1")} // 1 cpu, 1Gi already used
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{cpuQuota, memQuota}, existing)
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	big := model.Machine{
		Metadata: model.ObjectMeta{Name: "big", Namespace: "prod"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "1Gi"}},
	}
	if blocker := admitQuota(trackers, big); blocker == "" {
		t.Fatal("expected a 2-cpu request to be blocked when 1 cpu is already used against a maxTotalCpu of 2")
	}
}

func TestAdmitQuotaIgnoresNamespacesWithNoQuota(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(0)},
	}
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{quota}, nil)
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	other := model.Machine{Metadata: model.ObjectMeta{Name: "x", Namespace: "staging"}, Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}}
	if blocker := admitQuota(trackers, other); blocker != "" {
		t.Fatalf("expected no quota to apply to a different namespace, got %q", blocker)
	}
}

func TestBuildQuotaTrackersRejectsMalformedQuantities(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxTotalCPU: "not-a-number"},
	}
	if _, err := BuildQuotaTrackers([]model.MachineQuota{quota}, nil); err == nil {
		t.Fatal("expected an error for an invalid maxTotalCpu quantity")
	}
}

func TestAdmitQuotaDoesNotCountUnscheduledOrStoppedMachinesAsUsed(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(1)},
	}
	stopped := model.Machine{
		Metadata: model.ObjectMeta{Name: "stopped", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Stopped", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{quota}, []model.Machine{stopped, unscheduledMachine("pending")})
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("new")); blocker != "" {
		t.Fatalf("expected quota headroom since neither existing machine is actually running, got %q", blocker)
	}
}

// TestAdmitQuotaDoesNotCountHaltedMachinesAsUsed is Halted's own version of
// the Stopped case above -- both genuinely free the runtime/host footprint
// (see machineCountsTowardQuota's own doc comment), so neither should count
// against MaxMachines.
func TestAdmitQuotaDoesNotCountHaltedMachinesAsUsed(t *testing.T) {
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: intPtr(1)},
	}
	halted := model.Machine{
		Metadata: model.ObjectMeta{Name: "halted", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1", PowerState: "Halted", Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}},
	}
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{quota}, []model.Machine{halted})
	if err != nil {
		t.Fatalf("BuildQuotaTrackers: %v", err)
	}
	if blocker := admitQuota(trackers, unscheduledMachine("new")); blocker != "" {
		t.Fatalf("expected quota headroom since the existing machine is Halted, got %q", blocker)
	}
}
