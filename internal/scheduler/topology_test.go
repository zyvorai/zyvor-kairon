// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func intp(v int) *int { return &v }

// topoNode is a ready node whose two NUMA nodes each have cpusPerNUMA
// pinnable CPUs and one GPU.
func topoNode(name string, cpusPerNUMA int) (model.Node, model.NodeTopology) {
	n := node(name, true, true)
	last := 2*cpusPerNUMA - 1
	n.Metadata.Labels[model.PinnableCPUsLabel] = model.FormatCPUList(seq(0, last))
	nt := model.NodeTopology{Metadata: model.ObjectMeta{Name: name}}
	nt.Status.NUMABinding = true
	nt.Status.NUMANodes = []model.TopologyNUMANode{
		{ID: 0, CPUs: model.FormatCPUList(seq(0, cpusPerNUMA-1)), MemoryMiB: 16384},
		{ID: 1, CPUs: model.FormatCPUList(seq(cpusPerNUMA, last)), MemoryMiB: 16384},
	}
	nt.Status.Devices = []model.TopologyDevice{
		{BDF: "0000:3b:00.0", Class: "gpu", Vendor: "10de", NUMANode: intp(0), PCIeRoot: "pci0000:3a", Allocatable: true},
		{BDF: "0000:86:00.0", Class: "gpu", Vendor: "10de", NUMANode: intp(1), PCIeRoot: "pci0000:85", Allocatable: true},
	}
	return n, nt
}

func seq(from, to int) []uint32 {
	var out []uint32
	for i := from; i <= to; i++ {
		out = append(out, uint32(i))
	}
	return out
}

func topoMachine(cpu, policy string) model.Machine {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "default"}}
	m.Spec.Resources = model.ResourceSpec{CPU: cpu, Memory: "4Gi", CPUPinning: true}
	m.Spec.Topology = &model.TopologySpec{Policy: policy}
	return m
}

func TestChooseTopologyPrefersTheNodeThatFitsOneNUMANode(t *testing.T) {
	small, smallT := topoNode("small", 4)
	big, bigT := topoNode("big", 8)
	m := topoMachine("6", model.TopologyPolicyBestEffort)
	topo := TopologyInput{Nodes: map[string]model.NodeTopology{"small": smallT, "big": bigT}}
	got, err := Scheduler{}.ChooseTopology(m, []model.Node{small, big}, nil, LoadFromCounts(map[string]int{"big": 1}), topo)
	if err != nil || got != "big" {
		t.Fatalf("6 vCPUs fit one NUMA node only on big, which must win despite its load: got %q err=%v", got, err)
	}
}

func TestChooseTopologySingleNUMANodeIsAHardFilter(t *testing.T) {
	small, smallT := topoNode("small", 4)
	m := topoMachine("6", model.TopologyPolicySingleNUMANode)
	_, err := Scheduler{}.ChooseTopology(m, []model.Node{small}, nil, nil, TopologyInput{Nodes: map[string]model.NodeTopology{"small": smallT}})
	if err == nil || !strings.Contains(err.Error(), TopologyUnsatisfiable) || !strings.Contains(err.Error(), "no single NUMA node fits") {
		t.Fatalf("want a TopologyUnsatisfiable reason, got %v", err)
	}
}

func TestChooseTopologyNeedsNodeTopologyUnlessBestEffort(t *testing.T) {
	n := node("bare", true, true)
	n.Metadata.Labels[model.PinnableCPUsLabel] = "0-7"
	if _, err := (Scheduler{}).ChooseTopology(topoMachine("2", model.TopologyPolicyRestricted), []model.Node{n}, nil, nil, TopologyInput{}); err == nil || !strings.Contains(err.Error(), "no NodeTopology") {
		t.Fatalf("restricted must refuse a node without NodeTopology: %v", err)
	}
	if got, err := (Scheduler{}).ChooseTopology(topoMachine("2", model.TopologyPolicyBestEffort), []model.Node{n}, nil, nil, TopologyInput{}); err != nil || got != "bare" {
		t.Fatalf("best-effort without devices must still schedule: %q %v", got, err)
	}
}

func TestChooseTopologyDRAIsAHardConstraint(t *testing.T) {
	a, aT := topoNode("a", 8)
	b, bT := topoNode("b", 8)
	topo := TopologyInput{Nodes: map[string]model.NodeTopology{"a": aT, "b": bT}}
	m := topoMachine("2", model.TopologyPolicySingleNUMANode)

	topo.DRA = DRAPlacement{Pending: true}
	if _, err := (Scheduler{}).ChooseTopology(m, []model.Node{a, b}, nil, nil, topo); err == nil || !strings.HasPrefix(err.Error(), "DRAAllocationPending") {
		t.Fatalf("hard policy must wait for the claim: %v", err)
	}

	topo.DRA = DRAPlacement{Node: "b", BDFs: []string{"0000:86:00.0"}}
	got, err := Scheduler{}.ChooseTopology(m, []model.Node{a, b}, nil, LoadFromCounts(map[string]int{"b": 10}), topo)
	if err != nil || got != "b" {
		t.Fatalf("hard policy must follow the claim's node: %q %v", got, err)
	}
	alloc, _, reason, _ := AllocateTopology(m, b, nil, topo)
	if reason != "" || len(alloc.NUMANodes) != 1 || alloc.NUMANodes[0].ID != 1 {
		t.Fatalf("the claimed GPU sits on numa1, so the vCPUs must too: %+v %q", alloc, reason)
	}
}
