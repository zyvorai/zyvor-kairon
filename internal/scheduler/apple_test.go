// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func mac(name string, gen, macos string, rosetta, nested bool) model.Node {
	n := node(name, true, true)
	n.Metadata.Labels["kubernetes.io/arch"] = "arm64"
	n.Metadata.Labels[model.LabelBackendVZ] = "true"
	n.Metadata.Labels[model.LabelAppleChip] = "m" + gen
	n.Metadata.Labels[model.LabelAppleChipGeneration] = gen
	n.Metadata.Labels[model.LabelMacOSVersion] = macos
	if rosetta {
		n.Metadata.Labels[model.LabelVZRosetta] = "true"
	}
	if nested {
		n.Metadata.Labels[model.LabelVZNested] = "true"
	}
	n.Spec.Taints = []model.Taint{{Key: model.TaintVMOnly, Value: "true", Effect: model.TaintEffectNoSchedule}}
	return n
}

func TestAppleCapabilityScheduling(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	nodes := []model.Node{node("linux", true, true), mac("m1", "1", "14", false, false), mac("m4", "4", "27", true, true)}
	load := LoadFromCounts(map[string]int{})
	vz := func(a *model.AppleSpec) model.Machine {
		return model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "ai"}, Spec: model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "vz"}, Apple: a}}
	}

	// A vz Machine lands on a Mac without tolerating vm-only; nested needs the M4.
	if got, err := s.Choose(vz(&model.AppleSpec{NestedVirtualization: true}), nodes, nil, load, ""); err != nil || got != "m4" {
		t.Fatalf("nested: %q %v", got, err)
	}
	if got, err := s.Choose(vz(nil), []model.Node{nodes[0], nodes[1]}, nil, load, ""); err != nil || got != "m1" {
		t.Fatalf("plain vz: %q %v", got, err)
	}
	_, err := s.Choose(vz(&model.AppleSpec{MinChipGeneration: 5}), nodes, nil, load, "")
	if err == nil || !strings.Contains(err.Error(), "minChipGeneration") || !strings.Contains(err.Error(), "needs a Mac") {
		t.Fatalf("m5: %v", err)
	}

	// Other backends stay off Macs even when they tolerate the taint.
	q := model.Machine{Metadata: model.ObjectMeta{Name: "q"}, Spec: model.MachineSpec{Runtime: model.RuntimeSpec{Backend: "qemu"}}}
	q.Spec.Placement.Tolerations = []model.Toleration{{Key: model.TaintVMOnly, Operator: "Exists"}}
	if got, err := s.Choose(q, nodes, nil, load, ""); err != nil || got != "linux" {
		t.Fatalf("qemu: %q %v", got, err)
	}
	if _, err := s.Choose(q, nodes[1:], nil, load, ""); err == nil || !strings.Contains(err.Error(), "only the vz backend") {
		t.Fatalf("qemu on Macs only: %v", err)
	}
}

func TestMacNetworkMembershipScheduling(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	nodes := []model.Node{mac("a", "3", "15", true, true), mac("b", "4", "27", true, true)}
	net := model.MacNetwork{Metadata: model.ObjectMeta{Name: "shard", Namespace: "ai"}}
	net.Status.Members = []model.MacNetworkMember{{Node: "b", Hosts: [2]int{2, 31}}}
	topo := TopologyInput{MacNetworks: map[string]model.MacNetwork{"ai/shard": net}}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "ai"}, Spec: model.MachineSpec{Apple: &model.AppleSpec{Networks: []string{"shard"}}}}
	if got, err := s.ChooseTopology(m, nodes, nil, LoadFromCounts(nil), topo); err != nil || got != "b" {
		t.Fatalf("member: %q %v", got, err)
	}
	m.Spec.Apple.Networks = []string{"missing"}
	if _, err := s.ChooseTopology(m, nodes, nil, LoadFromCounts(nil), topo); err == nil || !strings.Contains(err.Error(), "MacNetwork missing not found") {
		t.Fatalf("missing: %v", err)
	}
}
