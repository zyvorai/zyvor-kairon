// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestPermittedDevices(t *testing.T) {
	zero := 0
	nt := model.NodeTopology{
		Metadata: model.ObjectMeta{Name: "n1"},
		Status: model.NodeTopologyStatus{
			Devices: []model.TopologyDevice{
				{BDF: "0000:01:00.0", Class: model.DeviceClassGPU, Vendor: "10de", Device: "2236", NUMANode: &zero, Allocatable: true, VRAMGiB: 24},
				{BDF: "0000:02:00.0", Class: model.DeviceClassGPU, Vendor: "10de", Device: "2236", Allocatable: true, Allocated: true},
				{BDF: "0000:03:00.1", Class: model.DeviceClassNIC, Vendor: "8086", Device: "154c", Allocatable: true, PhysFn: "0000:03:00.0"},
				{BDF: "0000:04:00.0", Class: model.DeviceClassGPU, Vendor: "10de", Device: "2236", Allocatable: true, Health: model.DeviceUnhealthy},
				{BDF: "0000:05:00.0", Class: model.DeviceClassNVMe, Vendor: "144d", Device: "a808"},
			},
			MediatedDevices: []model.TopologyMediatedParent{
				{Parent: "0000:06:00.0", Class: model.DeviceClassGPU, Allocatable: true, Types: []model.TopologyMediatedType{{ID: "nvidia-558", Name: "A100-3-20C", Available: 2}}},
				{Parent: "mbochs", Class: model.DeviceClassGPU, Types: []model.TopologyMediatedType{{ID: "mbochs-small", Available: 4}}},
			},
		},
	}
	got := permittedDevices([]model.NodeTopology{nt})
	rows := make([]string, 0, len(got))
	for _, d := range got {
		rows = append(rows, d.Kind+" "+d.ID+" free="+strings.Repeat("x", d.Free))
	}
	want := []string{
		"pci 0000:01:00.0 free=x",
		"pci 0000:02:00.0 free=",
		"pci 0000:04:00.0 free=",
		"sriov-vf 0000:03:00.1 free=x",
		"vgpu nvidia-558 free=xx",
	}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if got[0].NUMA != "0" || got[0].Note != "24GiB" || got[3].Parent != "0000:03:00.0" || got[4].Model != "A100-3-20C" {
		t.Fatalf("fields: %+v", got)
	}
	for _, f := range []string{"table", "json", "yaml"} {
		var buf bytes.Buffer
		if err := writePermittedDevices(&buf, got, f); err != nil || !strings.Contains(buf.String(), "nvidia-558") {
			t.Fatalf("%s: %v\n%s", f, err, buf.String())
		}
	}
	if err := writePermittedDevices(&bytes.Buffer{}, got, "dot"); err == nil {
		t.Fatal("unknown format accepted")
	}
}
