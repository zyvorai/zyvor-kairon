// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"reflect"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestNodeGPUs(t *testing.T) {
	if _, ok := nodeGPUs(model.NodeTopology{Status: model.NodeTopologyStatus{Devices: []model.TopologyDevice{{BDF: "0000:01:00.0", Class: "nic"}}}}, "n1", nil); ok {
		t.Fatal("a node without GPUs reported GPU metrics")
	}
	nt := model.NodeTopology{Status: model.NodeTopologyStatus{
		Devices: []model.TopologyDevice{
			{BDF: "0000:3b:00.0", Class: "gpu", VRAMGiB: 80, Health: model.DeviceHealthy},
			{BDF: "0000:86:00.0", Class: "gpu", VRAMGiB: 80, Health: model.DeviceUnhealthy},
			{BDF: "0000:af:00.0", Class: "gpu"},
		},
		MediatedDevices: []model.TopologyMediatedParent{{
			Parent: "0000:3b:00.0", Class: "gpu", Vendor: "10de", Health: model.DeviceHealthy,
			Types: []model.TopologyMediatedType{{ID: "nvidia-558", Available: 1, FramebufferMiB: 20480}},
		}},
	}}
	m := model.Machine{}
	m.Spec.NodeName = "n1"
	m.Spec.TopologyAllocation = &model.TopologyAllocation{Devices: []model.AllocatedDevice{
		{Name: "v", BDF: "0000:3b:00.0", Parent: "0000:3b:00.0", Profile: "nvidia-558", UUID: "u", FramebufferMiB: 20480},
	}}
	g, ok := nodeGPUs(nt, "n1", []model.Machine{m})
	if !ok {
		t.Fatal("no GPU metrics")
	}
	if want := map[string]int{"Healthy": 1, "Unhealthy": 1, "Unknown": 1}; !reflect.DeepEqual(g.ByHealth, want) {
		t.Errorf("by health %v", g.ByHealth)
	}
	if g.VGPUAvailable[[2]string{"0000:3b:00.0", "nvidia-558"}] != 1 {
		t.Errorf("vGPU available %v", g.VGPUAvailable)
	}
	if g.VRAMTotalMiB != 2*81920 || g.VRAMAllocatedMiB != 20480 {
		t.Errorf("VRAM total %d allocated %d", g.VRAMTotalMiB, g.VRAMAllocatedMiB)
	}
}
