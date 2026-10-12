// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

// Shapes as FluxVM serializes them (crates/fluxvm-core/src/{mdev,gpu_health}.rs).
const mdevJSON = `{"parents":[
 {"parent":"0000:3b:00.0","pci":true,"vendor_id":4318,"device_id":8373,"types":[
   {"id":"nvidia-558","name":"GRID A100-3-20C","device_api":"vfio-pci","available_instances":1,"framebuffer_mib":20480,"mig":{"slices":3,"memory_gib":20}}]},
 {"parent":"mbochs","pci":false,"types":[
   {"id":"mbochs-small","name":"mbochs-small","device_api":"vfio-pci","available_instances":63,"framebuffer_mib":4}]},
 {"parent":"mtty","pci":false,"types":[{"id":"mtty-1","device_api":"vfio-ccw","available_instances":24}]}],
 "instances":[{"uuid":"5A3C1D2E-0000-4000-8000-000000000001","parent":"mbochs","type_id":"mbochs-small","iommu_group":12,"allocated":true,"held":true}]}`

const healthJSON = `{"items":[
 {"bdf":"0000:3b:00.0","status":"healthy","link":{},"nvidia":{"memory_total_mib":81920,"mig_mode":"Enabled"}},
 {"bdf":"0000:86:00.0","status":"unhealthy","reasons":["1 fatal PCIe AER errors"],"link":{}}]}`

func intp(v int) *int { return &v }

func TestApplyGPUInventory(t *testing.T) {
	var mdev HostMdev
	if err := json.Unmarshal([]byte(mdevJSON), &mdev); err != nil {
		t.Fatal(err)
	}
	var health struct{ Items []HostGPUHealth }
	if err := json.Unmarshal([]byte(healthJSON), &health); err != nil {
		t.Fatal(err)
	}
	st := model.NodeTopologyStatus{Devices: []model.TopologyDevice{
		{BDF: "0000:3b:00.0", Class: "gpu", NUMANode: intp(0), PCIeRoot: "pci0000:3a", Allocatable: true},
		{BDF: "0000:86:00.0", Class: "gpu", NUMANode: intp(1), PCIeRoot: "pci0000:85", VRAMGiB: 80, Allocatable: true},
	}}
	ApplyGPUInventory(&st, &mdev, health.Items, map[string]struct{}{"0000:3b:00.0": {}})

	if d := st.Devices[0]; d.Health != model.DeviceHealthy || d.VRAMGiB != 80 {
		t.Errorf("device 0: %+v", d)
	}
	if d := st.Devices[1]; d.Health != model.DeviceUnhealthy || !reflect.DeepEqual(d.HealthReasons, []string{"1 fatal PCIe AER errors"}) {
		t.Errorf("device 1: %+v", d)
	}
	if len(st.MediatedDevices) != 2 {
		t.Fatalf("mediated %+v (mtty must be dropped)", st.MediatedDevices)
	}
	gpu, mb := st.MediatedDevices[0], st.MediatedDevices[1]
	want := model.TopologyMediatedParent{
		Parent: "0000:3b:00.0", Class: "gpu", Vendor: "10de", Device: "20b5", NUMANode: intp(0), PCIeRoot: "pci0000:3a",
		VRAMMiB: 81920, Health: model.DeviceHealthy, Allocatable: true,
		Types: []model.TopologyMediatedType{{ID: "nvidia-558", Name: "GRID A100-3-20C", Available: 1, FramebufferMiB: 20480, MIGSlices: 3, MIGMemoryGiB: 20}},
	}
	if !reflect.DeepEqual(gpu, want) {
		t.Errorf("gpu parent\n got %+v\nwant %+v", gpu, want)
	}
	if mb.Parent != "mbochs" || mb.Allocatable || mb.Vendor != "" || !reflect.DeepEqual(mb.Instances, []string{"5a3c1d2e-0000-4000-8000-000000000001"}) {
		t.Errorf("mbochs parent %+v", mb)
	}

	st2 := model.NodeTopologyStatus{}
	ApplyGPUInventory(&st2, &mdev, nil, nil)
	if !st2.MediatedDevices[0].Allocatable || !st2.MediatedDevices[1].Allocatable {
		t.Error("no --mdev-parents must allow every parent")
	}
}

func TestMdevClientRoutes(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		switch r.URL.Path {
		case "/v1/host/mdev":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, mdevJSON)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case "/v1/host/gpus/health":
			_, _ = io.WriteString(w, healthJSON)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	ctx := context.Background()
	if err := c.CreateMdev(ctx, "mbochs", "mbochs-small", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveMdev(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	inv, err := c.GetHostMdev(ctx)
	if err != nil || len(inv.Parents) != 3 || inv.Instances[0].TypeID != "mbochs-small" {
		t.Fatalf("%+v %v", inv, err)
	}
	h, err := c.GetHostGPUHealth(ctx)
	if err != nil || len(h) != 2 || h[1].Status != "unhealthy" {
		t.Fatalf("%+v %v", h, err)
	}
	want := []string{
		`POST /v1/host/mdev {"parent":"mbochs","type":"mbochs-small","uuid":"u1"}`,
		"DELETE /v1/host/mdev/u1 ",
		"GET /v1/host/mdev ",
		"GET /v1/host/gpus/health ",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests\n got %q\nwant %q", got, want)
	}
}
