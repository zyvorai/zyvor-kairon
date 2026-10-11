// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

func TestValidateTopology(t *testing.T) {
	pinned := func(policy string) MachineSpec {
		return MachineSpec{
			Topology:  &TopologySpec{Policy: policy},
			Resources: ResourceSpec{CPU: "4", Memory: "4Gi", CPUPinning: true},
		}
	}
	zero := 0
	cases := []struct {
		name string
		spec MachineSpec
		want string
	}{
		{"no topology", MachineSpec{}, ""},
		{"none without pinning", MachineSpec{Topology: &TopologySpec{Policy: "none"}}, ""},
		{"single-numa-node with pinning", pinned(TopologyPolicySingleNUMANode), ""},
		{"bad policy", pinned("strict"), "spec.topology.policy"},
		{"restricted without pinning", MachineSpec{Topology: &TopologySpec{Policy: "restricted"}}, "cpuPinning"},
		{"legacy numaNode", func() MachineSpec {
			s := pinned(TopologyPolicyRestricted)
			s.Resources.NUMANode = &zero
			return s
		}(), "replaces"},
		{"hugepageSize without hugepages", func() MachineSpec {
			s := pinned(TopologyPolicyRestricted)
			s.Topology.HugepageSize = "1Gi"
			return s
		}(), "hugepages: true"},
		{"hugepageSize with hugepages", func() MachineSpec {
			s := pinned(TopologyPolicyRestricted)
			s.Topology.HugepageSize = "2Mi"
			s.Resources.Hugepages = true
			return s
		}(), ""},
		{"gpu and nic aligned", MachineSpec{Devices: []MachineDevice{
			{Name: "gpu", Class: "gpu", Vendor: "nvidia", MinMemory: "40Gi", Alignment: []string{AlignSamePCIeRoot}},
			{Name: "nic", Class: "nic", Vendor: "15b3", VirtualFunction: true, Alignment: []string{AlignSamePCIeRoot}},
		}}, ""},
		{"devices on firecracker", MachineSpec{Runtime: RuntimeSpec{Backend: "firecracker"}, Devices: []MachineDevice{{Name: "g", Class: "gpu"}}}, "qemu backend"},
		{"duplicate name", MachineSpec{Devices: []MachineDevice{{Name: "g", Class: "gpu"}, {Name: "g", Class: "nic"}}}, "used twice"},
		{"bad class", MachineSpec{Devices: []MachineDevice{{Name: "g", Class: "fpga"}}}, "class"},
		{"bad vendor", MachineSpec{Devices: []MachineDevice{{Name: "g", Class: "gpu", Vendor: "acme"}}}, "vendor"},
		{"minMemory on nic", MachineSpec{Devices: []MachineDevice{{Name: "n", Class: "nic", MinMemory: "1Gi"}}}, "minMemory"},
		{"bad alignment", MachineSpec{Devices: []MachineDevice{{Name: "n", Class: "nic", Alignment: []string{"sameSocket"}}}}, "alignment"},
	}
	for _, c := range cases {
		err := ValidateTopology(c.spec)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestValidateTopologyUpdate(t *testing.T) {
	old := MachineSpec{Topology: &TopologySpec{Policy: "restricted"}, Devices: []MachineDevice{{Name: "g", Class: "gpu"}}}
	cur := old
	cur.Devices = []MachineDevice{{Name: "g", Class: "gpu", Count: 2}}
	if err := ValidateTopologyUpdate(old, cur); err != nil {
		t.Fatalf("unscheduled Machine: %v", err)
	}
	old.NodeName, cur.NodeName = "n1", "n1"
	if err := ValidateTopologyUpdate(old, cur); err == nil {
		t.Fatal("changing devices after scheduling was allowed")
	}
	cur.Devices = old.Devices
	cur.TopologyAllocation = &TopologyAllocation{NUMANodes: []AllocatedNUMANode{{ID: 0, CPUs: "2-3", MemoryMiB: 1024}}}
	if err := ValidateTopologyUpdate(old, cur); err != nil {
		t.Fatalf("allocation change refused: %v", err)
	}
}

func TestTopologyPolicyHelpers(t *testing.T) {
	var s MachineSpec
	if s.TopologyPolicy() != TopologyPolicyNone || s.NeedsTopology() || s.HardTopology() {
		t.Fatal("empty spec should not need topology")
	}
	s.Devices = []MachineDevice{{Name: "g", Class: "gpu"}}
	if !s.NeedsTopology() || s.HardTopology() {
		t.Fatal("devices alone need topology but are not hard")
	}
	s.Topology = &TopologySpec{Policy: TopologyPolicySingleNUMANode}
	if !s.HardTopology() {
		t.Fatal("single-numa-node is hard")
	}
	if PCIVendorID("nvidia") != "10de" || PCIVendorID("8086") != "8086" {
		t.Fatal("vendor names")
	}
}
