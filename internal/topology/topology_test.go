// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func intp(v int) *int { return &v }

// twoSocket: NUMA 0 has CPUs 0-7, NUMA 1 has 8-15, 16GiB each, 1Gi pages.
// A GPU and a NIC share root pci0000:3a on node 0; another GPU sits on its
// own root on node 1; the NIC on node 1 has two VFs.
func twoSocket() model.NodeTopology {
	return model.NodeTopology{
		Metadata: model.ObjectMeta{Name: "n1"},
		Status: model.NodeTopologyStatus{
			NUMABinding: true,
			NUMANodes: []model.TopologyNUMANode{
				{ID: 0, CPUs: "0-7", MemoryMiB: 16384, Hugepages: []model.TopologyHugepages{{Size: "1Gi", Total: 8, Free: 8}}},
				{ID: 1, CPUs: "8-15", MemoryMiB: 16384, Hugepages: []model.TopologyHugepages{{Size: "1Gi", Total: 8, Free: 8}}},
			},
			Devices: []model.TopologyDevice{
				{BDF: "0000:3b:00.0", Class: "gpu", Vendor: "10de", Device: "2330", NUMANode: intp(0), PCIeRoot: "pci0000:3a/0000:3a:00.0", VRAMGiB: 80, Allocatable: true},
				{BDF: "0000:3c:00.0", Class: "nic", Vendor: "15b3", Device: "101d", NUMANode: intp(0), PCIeRoot: "pci0000:3a/0000:3a:00.0", Allocatable: true},
				{BDF: "0000:86:00.0", Class: "gpu", Vendor: "10de", Device: "2330", NUMANode: intp(1), PCIeRoot: "pci0000:85/0000:85:00.0", VRAMGiB: 80, Allocatable: true},
				{BDF: "0000:af:00.0", Class: "nic", Vendor: "15b3", Device: "101d", NUMANode: intp(1), PCIeRoot: "pci0000:ae/0000:ae:00.0", TotalVFs: 2, Allocatable: true},
				{BDF: "0000:af:00.2", Class: "nic", Vendor: "15b3", Device: "101e", NUMANode: intp(1), PCIeRoot: "pci0000:ae/0000:ae:00.0", PhysFn: "0000:af:00.0", Allocatable: true},
				{BDF: "0000:af:00.3", Class: "nic", Vendor: "15b3", Device: "101e", NUMANode: intp(1), PCIeRoot: "pci0000:ae/0000:ae:00.0", PhysFn: "0000:af:00.0", Allocatable: true},
				{BDF: "0000:d8:00.0", Class: "nvme", Vendor: "8086", NUMANode: intp(1), PCIeRoot: "pci0000:d7", Allocatable: false},
			},
		},
	}
}

func node(pinnable string) model.Node {
	n := model.Node{}
	n.Metadata.Name = "n1"
	n.Metadata.Labels = map[string]string{model.PinnableCPUsLabel: pinnable}
	return n
}

func machine(name, cpu, mem, policy string, devs ...model.MachineDevice) model.Machine {
	m := model.Machine{}
	m.Metadata.Name = name
	m.Metadata.Namespace = "default"
	m.Spec.Resources = model.ResourceSpec{CPU: cpu, Memory: mem, CPUPinning: true}
	m.Spec.Topology = &model.TopologySpec{Policy: policy}
	m.Spec.Devices = devs
	return m
}

func allocate(t *testing.T, nt model.NodeTopology, pinnable string, others []model.Machine, m model.Machine) (Allocation, int, string) {
	t.Helper()
	r, err := RequestFor(m)
	if err != nil {
		t.Fatal(err)
	}
	return Allocate(BuildGraph(nt, node(pinnable), others, m), r)
}

func TestAllocateTable(t *testing.T) {
	gpuNIC := []model.MachineDevice{
		{Name: "gpu", Class: "gpu", Vendor: "nvidia", MinMemory: "40Gi", Alignment: []string{model.AlignSamePCIeRoot}},
		{Name: "nic", Class: "nic", Vendor: "mellanox", Alignment: []string{model.AlignSamePCIeRoot}},
	}
	busyNode0 := machine("busy", "6", "4Gi", model.TopologyPolicySingleNUMANode)
	busyNode0.Spec.NodeName = "n1"
	busyNode0.Spec.Resources.AllocatedCPUSet = []uint32{0, 1, 2, 3, 4, 5}
	busyNode0.Spec.TopologyAllocation = &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{{ID: 0, CPUs: "0-5", MemoryMiB: 4096}}}

	cases := []struct {
		name      string
		m         model.Machine
		others    []model.Machine
		pinnable  string
		wantNUMA  []model.AllocatedNUMANode
		wantDevs  []string
		wantScore int
		wantErr   string
	}{
		{
			name:      "single-numa-node fits on node 0",
			m:         machine("a", "4", "8Gi", model.TopologyPolicySingleNUMANode),
			wantNUMA:  []model.AllocatedNUMANode{{ID: 0, CPUs: "0-3", MemoryMiB: 8192}},
			wantScore: scoreOneNode,
		},
		{
			name:    "single-numa-node too big for one node",
			m:       machine("a", "10", "8Gi", model.TopologyPolicySingleNUMANode),
			wantErr: "no single NUMA node fits (numa0: 8 free pinnable CPUs, needs 10; numa1: 8 free pinnable CPUs, needs 10)",
		},
		{
			name:      "restricted spans two nodes",
			m:         machine("a", "10", "20Gi", model.TopologyPolicyRestricted),
			wantNUMA:  []model.AllocatedNUMANode{{ID: 0, CPUs: "0-4", MemoryMiB: 10240}, {ID: 1, CPUs: "8-12", MemoryMiB: 10240}},
			wantScore: scoreTwoNodes,
		},
		{
			name:     "fragmentation: best fit picks the fuller node",
			m:        machine("a", "2", "1Gi", model.TopologyPolicySingleNUMANode),
			others:   []model.Machine{busyNode0},
			wantNUMA: []model.AllocatedNUMANode{{ID: 0, CPUs: "6-7", MemoryMiB: 1024}},

			wantScore: scoreOneNode,
		},
		{
			name:     "restricted prefers one roomy node over spanning",
			m:        machine("a", "8", "8Gi", model.TopologyPolicyRestricted),
			others:   []model.Machine{busyNode0},
			wantNUMA: []model.AllocatedNUMANode{{ID: 1, CPUs: "8-15", MemoryMiB: 8192}},

			wantScore: scoreOneNode,
		},
		{
			name:      "gpu and nic on one PCIe root",
			m:         machine("a", "4", "8Gi", model.TopologyPolicySingleNUMANode, gpuNIC...),
			wantNUMA:  []model.AllocatedNUMANode{{ID: 0, CPUs: "0-3", MemoryMiB: 8192}},
			wantDevs:  []string{"gpu=0000:3b:00.0", "nic=0000:3c:00.0"},
			wantScore: scoreOneNode,
		},
		{
			name: "gpu and nic follow the root to node 1 when node 0's CPUs are taken",
			m: machine("a", "4", "8Gi", model.TopologyPolicySingleNUMANode,
				model.MachineDevice{Name: "gpu", Class: "gpu", Alignment: []string{model.AlignSameNUMA}}),
			others:    []model.Machine{busyNode0},
			wantNUMA:  []model.AllocatedNUMANode{{ID: 1, CPUs: "8-11", MemoryMiB: 8192}},
			wantDevs:  []string{"gpu=0000:86:00.0"},
			wantScore: scoreOneNode,
		},
		{
			name:      "virtual functions come from the PF's node",
			m:         machine("a", "2", "2Gi", model.TopologyPolicySingleNUMANode, model.MachineDevice{Name: "vf", Class: "nic", VirtualFunction: true, Count: 2}),
			wantNUMA:  []model.AllocatedNUMANode{{ID: 1, CPUs: "8-9", MemoryMiB: 2048}},
			wantDevs:  []string{"vf=0000:af:00.2", "vf=0000:af:00.3"},
			wantScore: scoreOneNode,
		},
		{
			name:    "a disallowed device is never picked",
			m:       machine("a", "2", "2Gi", model.TopologyPolicySingleNUMANode, model.MachineDevice{Name: "disk", Class: "nvme"}),
			wantErr: "needs 1 nvme, 0 free",
		},
		{
			name:     "only the pinnable CPUs count",
			m:        machine("a", "4", "1Gi", model.TopologyPolicySingleNUMANode),
			pinnable: "2-3_10-11",
			wantErr:  "numa0: 2 free pinnable CPUs, needs 4",
		},
		{
			name: "best-effort falls back to remote devices",
			m: machine("a", "8", "8Gi", model.TopologyPolicyBestEffort,
				model.MachineDevice{Name: "gpu", Class: "gpu", Count: 2}),
			wantNUMA:  []model.AllocatedNUMANode{{ID: 0, CPUs: "0-3", MemoryMiB: 4096}, {ID: 1, CPUs: "8-11", MemoryMiB: 4096}},
			wantDevs:  []string{"gpu=0000:3b:00.0", "gpu=0000:86:00.0"},
			wantScore: scoreTwoNodes,
		},
		{
			name:      "policy none only picks devices",
			m:         machine("a", "2", "1Gi", model.TopologyPolicyNone, model.MachineDevice{Name: "gpu", Class: "gpu", Model: "2330"}),
			wantDevs:  []string{"gpu=0000:3b:00.0"},
			wantScore: scoreNoNUMAPlan,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin := c.pinnable
			if pin == "" {
				pin = "0-15"
			}
			a, score, reason := allocate(t, twoSocket(), pin, c.others, c.m)
			if c.wantErr != "" {
				if !strings.Contains(reason, c.wantErr) {
					t.Fatalf("reason %q, want it to contain %q", reason, c.wantErr)
				}
				return
			}
			if reason != "" {
				t.Fatalf("unexpected failure: %s", reason)
			}
			if !reflect.DeepEqual(a.NUMANodes, c.wantNUMA) {
				t.Errorf("numa %+v, want %+v", a.NUMANodes, c.wantNUMA)
			}
			var devs []string
			for _, d := range a.Devices {
				devs = append(devs, d.Name+"="+d.BDF)
			}
			if !reflect.DeepEqual(devs, c.wantDevs) {
				t.Errorf("devices %v, want %v", devs, c.wantDevs)
			}
			if score != c.wantScore {
				t.Errorf("score %d, want %d", score, c.wantScore)
			}
		})
	}
}

func TestHugepagesPerNode(t *testing.T) {
	m := machine("a", "4", "6Gi", model.TopologyPolicySingleNUMANode)
	m.Spec.Resources.Hugepages = true
	m.Spec.Topology.HugepageSize = "1Gi"
	holder := machine("h", "2", "6Gi", model.TopologyPolicySingleNUMANode)
	holder.Spec.Resources.Hugepages = true
	holder.Spec.Topology.HugepageSize = "1Gi"
	holder.Spec.NodeName = "n1"
	holder.Spec.Resources.AllocatedCPUSet = []uint32{0, 1}
	holder.Spec.TopologyAllocation = &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{{ID: 0, CPUs: "0-1", MemoryMiB: 6144, Hugepages: 6}}}

	a, _, reason := allocate(t, twoSocket(), "0-15", []model.Machine{holder}, m)
	if reason != "" {
		t.Fatal(reason)
	}
	want := []model.AllocatedNUMANode{{ID: 1, CPUs: "8-11", MemoryMiB: 6144, Hugepages: 6}}
	if !reflect.DeepEqual(a.NUMANodes, want) {
		t.Fatalf("numa %+v, want %+v (node 0 has only 2 pages left)", a.NUMANodes, want)
	}

	odd := machine("o", "2", "1536Mi", model.TopologyPolicySingleNUMANode)
	odd.Spec.Resources.Hugepages = true
	odd.Spec.Topology.HugepageSize = "1Gi"
	if _, _, reason := allocate(t, twoSocket(), "0-15", nil, odd); !strings.Contains(reason, "not a multiple") {
		t.Fatalf("reason %q", reason)
	}
}

func TestDRAAndBinding(t *testing.T) {
	m := machine("a", "2", "1Gi", model.TopologyPolicySingleNUMANode)
	r, _ := RequestFor(m)
	g := BuildGraph(twoSocket(), node("0-15"), nil, m)
	r.RequiredNUMA = NodesForBDFs(twoSocket(), []string{"0000:86:00.0"})
	a, _, reason := Allocate(g, r)
	if reason != "" || a.NUMANodes[0].ID != 1 {
		t.Fatalf("DRA device on node 1: got %+v %q", a.NUMANodes, reason)
	}

	nt := twoSocket()
	nt.Status.NUMABinding = false
	if _, _, reason := Allocate(BuildGraph(nt, node("0-15"), nil, m), r); !strings.Contains(reason, "cannot bind") {
		t.Fatalf("reason %q", reason)
	}
}

func TestGraphSkipsHeldDevicesAndSelf(t *testing.T) {
	m := machine("a", "2", "1Gi", model.TopologyPolicySingleNUMANode)
	m.Spec.NodeName = "n1"
	m.Spec.Resources.AllocatedCPUSet = []uint32{0, 1}
	other := machine("b", "2", "1Gi", model.TopologyPolicySingleNUMANode)
	other.Spec.NodeName = "n1"
	other.Spec.TopologyAllocation = &model.TopologyAllocation{Devices: []model.AllocatedDevice{{Name: "g", BDF: "0000:3b:00.0"}}}
	nt := twoSocket()
	nt.Status.Devices[1].Allocated = true

	g := BuildGraph(nt, node("0-15"), []model.Machine{m, other}, m)
	if got := len(g.NUMANodes[0].FreeCPUs); got != 8 {
		t.Fatalf("self's own CPUs must stay free: %d", got)
	}
	for _, d := range g.Devices {
		if d.BDF == "0000:3b:00.0" || d.BDF == "0000:3c:00.0" || d.BDF == "0000:d8:00.0" {
			t.Fatalf("held, allocated or disallowed device %s in graph", d.BDF)
		}
	}
}

func TestAllocateShapeForLiveMigration(t *testing.T) {
	m := machine("a", "6", "6Gi", model.TopologyPolicyRestricted)
	busy := machine("busy", "6", "1Gi", model.TopologyPolicySingleNUMANode)
	busy.Spec.NodeName = "n1"
	busy.Spec.Resources.AllocatedCPUSet = []uint32{0, 1, 2, 3, 4, 5}
	g := BuildGraph(twoSocket(), node("0-15"), []model.Machine{busy}, m)
	shape := []model.AllocatedNUMANode{{ID: 0, CPUs: "0-3", MemoryMiB: 4096}, {ID: 1, CPUs: "8-10", MemoryMiB: 2048}}

	if _, reason := AllocateShape(g, shape, ""); !strings.Contains(reason, "guest node 1") {
		t.Fatalf("node 0 has 2 free CPUs and node 1 takes guest node 0, so guest node 1 cannot fit: %q", reason)
	}
	a, reason := AllocateShape(g, []model.AllocatedNUMANode{{ID: 1, CPUs: "8-9", MemoryMiB: 2048}}, "")
	if reason != "" {
		t.Fatal(reason)
	}
	want := []model.AllocatedNUMANode{{ID: 0, CPUs: "6-7", MemoryMiB: 2048}}
	if !reflect.DeepEqual(a.NUMANodes, want) {
		t.Fatalf("got %+v, want %+v", a.NUMANodes, want)
	}
}

func TestAllocateIsDeterministic(t *testing.T) {
	m := machine("a", "6", "12Gi", model.TopologyPolicyRestricted)
	first, _, _ := allocate(t, twoSocket(), "0-15", nil, m)
	for i := 0; i < 20; i++ {
		again, _, _ := allocate(t, twoSocket(), "0-15", nil, m)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs: %+v vs %+v", i, first, again)
		}
	}
}
