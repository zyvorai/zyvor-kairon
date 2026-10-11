// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestNodeTopologyStatusFromFluxVM(t *testing.T) {
	raw := `{"numaNodes":[{"id":0,"cpus":[0,1,2,3,8,9],"threadSiblings":[],"memoryMib":16384,"freeMemoryMib":9000,
	  "hugepages":[{"sizeKib":2048,"total":512,"free":500},{"sizeKib":1048576,"total":4,"free":4},{"sizeKib":64,"total":1,"free":1}]}],
	  "devices":[
	    {"bdf":"0000:3b:00.0","class":"gpu","classId":196608,"vendorId":4318,"deviceId":8992,"numaNode":0,"pcieRoot":"pci0000:3a","iommuGroup":7,"vramGib":80,"allocated":false},
	    {"bdf":"0000:af:00.0","class":"nic","classId":131072,"vendorId":5555,"deviceId":4125,"numaNode":0,"pcieRoot":"pci0000:ae","sriov":{"totalVfs":8,"numVfs":2,"vfs":["0000:af:00.2"]},"netdevs":["ens1f0"],"linkSpeedMbps":100000,"allocated":true}]}`
	var host HostTopology
	if err := json.Unmarshal([]byte(raw), &host); err != nil {
		t.Fatal(err)
	}
	st := NodeTopologyStatus(&host, true, map[string]struct{}{"0000:3b:00.0": {}})
	if !st.NUMABinding || len(st.NUMANodes) != 1 {
		t.Fatalf("%+v", st)
	}
	n := st.NUMANodes[0]
	if n.CPUs != "0-3,8-9" || n.MemoryMiB != 16384 || n.FreeMemoryMiB != 9000 {
		t.Fatalf("numa node: %+v", n)
	}
	wantHP := []model.TopologyHugepages{{Size: "2Mi", Total: 512, Free: 500}, {Size: "1Gi", Total: 4, Free: 4}}
	if !reflect.DeepEqual(n.Hugepages, wantHP) {
		t.Fatalf("hugepages: %+v", n.Hugepages)
	}
	gpu, nic := st.Devices[0], st.Devices[1]
	if gpu.Vendor != "10de" || gpu.Device != "2320" || !gpu.Allocatable || gpu.VRAMGiB != 80 || *gpu.IOMMUGroup != 7 {
		t.Fatalf("gpu: %+v", gpu)
	}
	if nic.Vendor != "15b3" || nic.Allocatable || !nic.Allocated || nic.TotalVFs != 8 || nic.LinkSpeedMbps != 100000 {
		t.Fatalf("nic outside the allowlist must not be allocatable: %+v", nic)
	}
}

func TestBuildCreateRequestSendsNUMAFromTopologyAllocation(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "default"}}
	m.Spec.Image.Path = "/img"
	m.Spec.Resources = model.ResourceSpec{CPU: "4", Memory: "4Gi", CPUPinning: true}
	m.Spec.Topology = &model.TopologySpec{Policy: model.TopologyPolicyRestricted}
	m.Spec.TopologyAllocation = &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{
		{ID: 0, CPUs: "2-3", MemoryMiB: 2048},
		{ID: 1, CPUs: "8,10", MemoryMiB: 2048},
	}}
	req, err := buildCreateRequest(m, "qemu", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []NUMANode{
		{HostNode: 0, VCPUs: []uint32{0, 1}, HostCPUs: []uint32{2, 3}, MemoryMiB: 2048},
		{HostNode: 1, VCPUs: []uint32{2, 3}, HostCPUs: []uint32{8, 10}, MemoryMiB: 2048},
	}
	if !reflect.DeepEqual(req.NUMA, want) {
		t.Fatalf("numa: %+v", req.NUMA)
	}

	m.Spec.Resources.CPU = "6"
	if _, err := buildCreateRequest(m, "qemu", nil, nil); err == nil || !strings.Contains(err.Error(), "pins 4 vCPUs") {
		t.Fatalf("a vCPU count that disagrees with the allocation must fail: %v", err)
	}
	m.Spec.Resources.CPU = "4"
	if _, err := buildCreateRequest(m, "cloud-hypervisor", nil, nil); err == nil || !strings.Contains(err.Error(), "qemu backend") {
		t.Fatalf("NUMA binding needs qemu: %v", err)
	}
}
