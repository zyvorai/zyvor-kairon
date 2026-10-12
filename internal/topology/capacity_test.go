// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func profileDevices(spec model.MachineAcceleratorProfileSpec) []model.MachineDevice {
	return spec.Devices("acc")
}

func TestDeviceCapacity(t *testing.T) {
	gpu := model.MachineAcceleratorProfileSpec{Vendor: "nvidia"}
	pairP2P := model.MachineAcceleratorProfileSpec{Vendor: "nvidia", Count: 2, Topology: &model.AcceleratorTopology{P2PRequired: true}}
	pairAnywhere := model.MachineAcceleratorProfileSpec{Vendor: "nvidia", Count: 2}
	withNIC := model.MachineAcceleratorProfileSpec{Vendor: "nvidia", NIC: &model.AcceleratorNIC{Vendor: "mellanox"}}
	bigVRAM := model.MachineAcceleratorProfileSpec{Vendor: "nvidia", Memory: &model.AcceleratorMemory{MinimumPerDevice: "96Gi"}}
	smallPairOnOneParent := model.MachineAcceleratorProfileSpec{Profile: "mbochs-large", Count: 2, Topology: &model.AcceleratorTopology{P2PRequired: true}}

	cases := []struct {
		name       string
		nt         model.NodeTopology
		others     []model.Machine
		spec       model.MachineAcceleratorProfileSpec
		want       int
		wantReason string
	}{
		{name: "one GPU each", nt: twoSocket(), spec: gpu, want: 2},
		{name: "two GPUs anywhere", nt: twoSocket(), spec: pairAnywhere, want: 1},
		{name: "two GPUs under one root", nt: twoSocket(), spec: pairP2P, want: 0, wantReason: "no PCIe root has 2 gpu vendor nvidia free"},
		{name: "GPU with a NIC on its root", nt: twoSocket(), spec: withNIC, want: 1},
		{name: "too little VRAM", nt: twoSocket(), spec: bigVRAM, want: 0, wantReason: "with 96Gi"},
		{
			name: "held GPU is not counted", nt: twoSocket(), spec: gpu, want: 1,
			others: []model.Machine{placed(plain("held"), model.AllocatedDevice{Name: "gpu", BDF: "0000:3b:00.0"})},
		},
		{name: "vGPU pairs on a virtual parent", nt: vgpuNode(), spec: smallPairOnOneParent, want: 2},
		{
			name: "vGPU instances other Machines hold", nt: vgpuNode(), spec: smallPairOnOneParent, want: 1,
			others: []model.Machine{placed(plain("held"),
				model.AllocatedDevice{Name: "g", Parent: "mbochs", Profile: "mbochs-large", UUID: "u1", FramebufferMiB: 64},
				model.AllocatedDevice{Name: "g", Parent: "mbochs", Profile: "mbochs-large", UUID: "u2", FramebufferMiB: 64},
			)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := BuildGraph(tc.nt, node("0-15"), tc.others, model.Machine{})
			got, reason := DeviceCapacity(g, profileDevices(tc.spec), 16)
			if got != tc.want {
				t.Fatalf("fits %d, want %d (reason %q)", got, tc.want, reason)
			}
			if tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason %q, want it to contain %q", reason, tc.wantReason)
			}
			again, _ := DeviceCapacity(g, profileDevices(tc.spec), 16)
			if again != got {
				t.Fatal("DeviceCapacity changed the graph it was given")
			}
		})
	}
}

func TestDeviceCapacityStopsAtLimit(t *testing.T) {
	g := BuildGraph(vgpuNode(), node("0-15"), nil, model.Machine{})
	got, reason := DeviceCapacity(g, profileDevices(model.MachineAcceleratorProfileSpec{Profile: "mbochs-small"}), 5)
	if got != 5 || reason != "" {
		t.Fatalf("fits %d reason %q, want 5 and no reason", got, reason)
	}
}

// A p2p pair of vGPUs on the virtual mbochs parent is placed on that one
// parent, which has no PCIe root.
func TestSamePCIeRootOnVirtualMediatedParent(t *testing.T) {
	m := plain("a", model.MachineAcceleratorProfileSpec{
		Profile: "mbochs-small", Count: 2, Topology: &model.AcceleratorTopology{P2PRequired: true},
	}.Devices("gpu")...)
	a, _, reason := allocate(t, vgpuNode(), "0-15", nil, m)
	if reason != "" {
		t.Fatal(reason)
	}
	if len(a.Devices) != 2 || a.Devices[0].Parent != "mbochs" || a.Devices[1].Parent != "mbochs" || a.Devices[0].PCIeRoot != "" {
		t.Fatalf("devices %+v", a.Devices)
	}
}
