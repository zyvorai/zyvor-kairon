// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

// vgpuNode is twoSocket where the GPU at 0000:3b:00.0 runs the vGPU host
// driver (two MIG-backed 20GiB profiles or one 40GiB), plus the kernel's
// virtual mbochs parent with a 256MiB pool.
func vgpuNode() model.NodeTopology {
	nt := twoSocket()
	nt.Status.MediatedDevices = []model.TopologyMediatedParent{
		{
			Parent: "0000:3b:00.0", Class: "gpu", Vendor: "10de", Device: "20b5", NUMANode: intp(0),
			PCIeRoot: "pci0000:3a/0000:3a:00.0", Allocatable: true,
			Types: []model.TopologyMediatedType{
				{ID: "nvidia-558", Name: "GRID A100-3-20C", Available: 2, FramebufferMiB: 20480, MIGSlices: 3, MIGMemoryGiB: 20},
				{ID: "nvidia-560", Name: "GRID A100-7-40C", Available: 1, FramebufferMiB: 40960, MIGSlices: 7, MIGMemoryGiB: 40},
			},
		},
		{
			Parent: "mbochs", Class: "gpu", Allocatable: true,
			Types: []model.TopologyMediatedType{
				{ID: "mbochs-large", Name: "mbochs-large", Available: 4, FramebufferMiB: 64},
				{ID: "mbochs-small", Name: "mbochs-small", Available: 64, FramebufferMiB: 4},
			},
		},
	}
	return nt
}

func vgpu(name, profile string, count int) model.MachineDevice {
	return model.MachineDevice{Name: name, Class: "gpu", Profile: profile, Count: count}
}

func plain(name string, devs ...model.MachineDevice) model.Machine {
	m := machine(name, "2", "2Gi", model.TopologyPolicyNone, devs...)
	m.Metadata.UID = "uid-" + name
	return m
}

func placed(m model.Machine, devs ...model.AllocatedDevice) model.Machine {
	m.Spec.NodeName = "n1"
	m.Spec.TopologyAllocation = &model.TopologyAllocation{Devices: devs}
	return m
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestProfileByNamePicksInstancesWithStableUUIDs(t *testing.T) {
	m := plain("a", vgpu("gpu", "grid a100-3-20c", 2))
	a, _, reason := allocate(t, vgpuNode(), "0-15", nil, m)
	if reason != "" {
		t.Fatal(reason)
	}
	if len(a.Devices) != 2 {
		t.Fatalf("devices %+v", a.Devices)
	}
	for i, d := range a.Devices {
		if d.Parent != "0000:3b:00.0" || d.BDF != "0000:3b:00.0" || d.Profile != "nvidia-558" || d.FramebufferMiB != 20480 || *d.NUMANode != 0 {
			t.Errorf("device %d: %+v", i, d)
		}
		if !uuidRE.MatchString(d.UUID) || d.UUID != MediatedUUID("uid-a", "gpu", i) {
			t.Errorf("device %d UUID %q", i, d.UUID)
		}
	}
	if a.Devices[0].UUID == a.Devices[1].UUID {
		t.Fatal("two instances share a UUID")
	}
	again, _, _ := allocate(t, vgpuNode(), "0-15", nil, m)
	if again.Devices[1].UUID != a.Devices[1].UUID {
		t.Fatal("placing the same Machine again changed its UUIDs")
	}
	if _, _, reason := allocate(t, vgpuNode(), "0-15", nil, plain("b", vgpu("gpu", "nvidia-558", 3))); !strings.Contains(reason, "profile nvidia-558, 2 free") {
		t.Fatalf("reason %q", reason)
	}
}

func TestAVGPUParentIsNotAlsoPassedThroughWhole(t *testing.T) {
	whole := model.MachineDevice{Name: "gpu", Class: "gpu", Vendor: "nvidia", Count: 2}
	if _, _, reason := allocate(t, vgpuNode(), "0-15", nil, plain("a", whole)); !strings.Contains(reason, "1 free") {
		t.Fatalf("reason %q", reason)
	}
	whole.Count = 1
	a, _, reason := allocate(t, vgpuNode(), "0-15", nil, plain("a", whole))
	if reason != "" || a.Devices[0].BDF != "0000:86:00.0" || a.Devices[0].Mediated() {
		t.Fatalf("%+v %q", a.Devices, reason)
	}
}

func TestPendingInstancesOfOtherMachinesCount(t *testing.T) {
	nt := vgpuNode()
	held := model.AllocatedDevice{Name: "gpu", BDF: "0000:3b:00.0", Parent: "0000:3b:00.0", Profile: "nvidia-558", UUID: MediatedUUID("uid-x", "gpu", 0), FramebufferMiB: 20480}
	others := []model.Machine{placed(plain("x"), held)}

	// Not created yet: the parent still says 2 available, but one is spoken for.
	if _, _, reason := allocate(t, nt, "0-15", others, plain("a", vgpu("gpu", "nvidia-558", 2))); !strings.Contains(reason, "1 free") {
		t.Fatalf("reason %q", reason)
	}
	if _, _, reason := allocate(t, nt, "0-15", others, plain("a", vgpu("gpu", "nvidia-560", 1))); !strings.Contains(reason, "0 free") {
		t.Fatalf("a 40GiB profile fit next to a pending 20GiB one: %q", reason)
	}

	// Created: the parent lists it and its count already went down.
	nt.Status.MediatedDevices[0].Instances = []string{strings.ToUpper(held.UUID)}
	nt.Status.MediatedDevices[0].Types[0].Available = 1
	nt.Status.MediatedDevices[0].Types[1].Available = 0
	if _, _, reason := allocate(t, nt, "0-15", others, plain("a", vgpu("gpu", "nvidia-558", 1))); reason != "" {
		t.Fatal(reason)
	}
	if _, _, reason := allocate(t, nt, "0-15", others, plain("a", vgpu("gpu", "nvidia-558", 2))); reason == "" {
		t.Fatal("a created instance was counted as free")
	}
}

func TestOneRequestSharesAParentPool(t *testing.T) {
	m := plain("a", vgpu("big", "mbochs-large", 4), vgpu("small", "mbochs-small", 1))
	if _, _, reason := allocate(t, vgpuNode(), "0-15", nil, m); !strings.Contains(reason, "profile mbochs-small, 0 free") {
		t.Fatalf("reason %q", reason)
	}
	m = plain("a", vgpu("big", "mbochs-large", 3), vgpu("small", "mbochs-small", 16))
	a, _, reason := allocate(t, vgpuNode(), "0-15", nil, m)
	if reason != "" || len(a.Devices) != 19 {
		t.Fatalf("%d devices, %q", len(a.Devices), reason)
	}
	for _, d := range a.Devices {
		if d.BDF != "" || d.Parent != "mbochs" || d.NUMANode != nil {
			t.Fatalf("virtual parent allocation %+v", d)
		}
	}
}

func TestMinMemoryAndHardPolicyAgainstProfiles(t *testing.T) {
	d := vgpu("gpu", "nvidia-558", 1)
	d.MinMemory = "40Gi"
	if _, _, reason := allocate(t, vgpuNode(), "0-15", nil, plain("a", d)); !strings.Contains(reason, "0 free") {
		t.Fatalf("a 20GiB profile met minMemory 40Gi: %q", reason)
	}
	// A virtual parent has no NUMA node, so a hard policy cannot place it locally.
	m := machine("a", "2", "2Gi", model.TopologyPolicySingleNUMANode, vgpu("gpu", "mbochs-small", 1))
	if _, _, reason := allocate(t, vgpuNode(), "0-15", nil, m); reason == "" {
		t.Fatal("virtual parent placed under single-numa-node")
	}
	m = machine("a", "2", "2Gi", model.TopologyPolicySingleNUMANode, vgpu("gpu", "nvidia-558", 1))
	a, _, reason := allocate(t, vgpuNode(), "0-15", nil, m)
	if reason != "" || a.NUMANodes[0].ID != 0 {
		t.Fatalf("%+v %q", a, reason)
	}
}

func TestUnhealthyAndDisallowedParentsAreSkipped(t *testing.T) {
	nt := vgpuNode()
	nt.Status.MediatedDevices[0].Health = model.DeviceUnhealthy
	nt.Status.MediatedDevices[1].Allocatable = false
	nt.Status.Devices[2].Health = model.DeviceUnhealthy
	g := BuildGraph(nt, node("0-15"), nil, model.Machine{})
	if len(g.Mediated) != 0 {
		t.Fatalf("mediated %+v", g.Mediated)
	}
	for _, d := range g.Devices {
		if d.BDF == "0000:86:00.0" || d.BDF == "0000:3b:00.0" {
			t.Fatalf("device %s is in the graph", d.BDF)
		}
	}
	nt.Status.Devices[2].Health = model.DeviceDegraded
	if len(BuildGraph(nt, node("0-15"), nil, model.Machine{}).Devices) != 5 {
		t.Fatal("a Degraded GPU was dropped")
	}
}

func TestVRAMUsage(t *testing.T) {
	nt := vgpuNode()
	u := MediatedUUID("uid-x", "small", 0)
	nt.Status.MediatedDevices[1].Instances = []string{u}
	nt.Status.MediatedDevices[1].Types[0].Available = 3
	nt.Status.MediatedDevices[1].Types[1].Available = 63
	x := placed(plain("x"),
		model.AllocatedDevice{Name: "gpu", BDF: "0000:86:00.0"},
		model.AllocatedDevice{Name: "v", BDF: "0000:3b:00.0", Parent: "0000:3b:00.0", Profile: "nvidia-558", UUID: MediatedUUID("uid-x", "v", 0), FramebufferMiB: 20480},
		model.AllocatedDevice{Name: "small", Parent: "mbochs", Profile: "mbochs-small", UUID: u, FramebufferMiB: 4},
	)
	gone := placed(plain("gone"), model.AllocatedDevice{Name: "gpu", BDF: "0000:86:00.0"})
	gone.Spec.NodeName = "n2"
	v := VRAMUsage(nt, "n1", []model.Machine{x, gone})
	// Two 80GiB GPUs (one as a vGPU parent) and mbochs: 252MiB free + 4MiB used.
	if want := (VRAM{TotalMiB: 2*81920 + 256, AllocatedMiB: 81920 + 20480 + 4}); v != want {
		t.Fatalf("VRAMUsage = %+v, want %+v", v, want)
	}
}
