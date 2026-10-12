// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"regexp"
	"strings"
	"testing"
)

func TestParseAppleChip(t *testing.T) {
	for brand, want := range map[string]struct {
		label string
		gen   int
	}{
		"Apple M1":       {"m1", 1},
		"Apple M3 Pro":   {"m3-pro", 3},
		"Apple M4 Max":   {"m4-max", 4},
		"Apple M2 Ultra": {"m2-ultra", 2},
		" Apple M5 ":     {"m5", 5},
	} {
		l, g, ok := ParseAppleChip(brand)
		if !ok || l != want.label || g != want.gen {
			t.Errorf("%q = %q %d %v", brand, l, g, ok)
		}
	}
	for _, brand := range []string{"Intel(R) Core(TM) i9-9980HK", "", "Apple"} {
		if _, _, ok := ParseAppleChip(brand); ok {
			t.Errorf("%q parsed", brand)
		}
	}
	if MacOSMajor("27.2") != "27" || MacOSMajor("15.6.1") != "15" || MacOSMajor("") != "" {
		t.Fatal("MacOSMajor")
	}
}

func TestAppleNodeReason(t *testing.T) {
	m4 := map[string]string{LabelAppleChip: "m4-max", LabelAppleChipGeneration: "4", LabelMacOSVersion: "27", LabelVZRosetta: "true", LabelVZNested: "true"}
	m1 := map[string]string{LabelAppleChip: "m1", LabelAppleChipGeneration: "1", LabelMacOSVersion: "14"}
	all := &AppleSpec{MinChipGeneration: 3, MinMacOSVersion: 15, Rosetta: true, NestedVirtualization: true}
	if r := AppleNodeReason(all, m4); r != "" {
		t.Fatalf("m4: %s", r)
	}
	for spec, want := range map[*AppleSpec]string{
		{MinChipGeneration: 3}:       "minChipGeneration",
		{MinMacOSVersion: 15}:        "minMacOSVersion",
		{Rosetta: true}:              "rosetta",
		{NestedVirtualization: true}: "nestedVirtualization",
	} {
		if r := AppleNodeReason(spec, m1); !strings.Contains(r, want) {
			t.Errorf("%+v on m1: %q", spec, r)
		}
	}
	if AppleNodeReason(nil, m1) != "" || AppleNodeReason(&AppleSpec{}, nil) != "" {
		t.Fatal("no requirements")
	}
}

func TestMacNetworkName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	a, b := MacNetworkName("team-a", "shard.net"), MacNetworkName("team-b", "shard.net")
	if a == b || !valid.MatchString(a) {
		t.Fatalf("%q %q", a, b)
	}
	long := MacNetworkName("ns", strings.Repeat("a", 60)+"-b")
	if len(long) > 40 || !valid.MatchString(long) {
		t.Fatalf("%q", long)
	}
}

func macNode(name, fabric, tb, ip string) Node {
	n := Node{Metadata: ObjectMeta{Name: name, Labels: map[string]string{LabelBackendVZ: "true"}, Annotations: map[string]string{}}}
	if fabric != "" {
		n.Metadata.Labels[LabelThunderboltFabric] = fabric
	}
	if tb != "" {
		n.Metadata.Annotations[AnnotationThunderboltAddresses] = tb
	}
	if ip != "" {
		n.Status.Addresses = []NodeAddress{{Type: "InternalIP", Address: ip}}
	}
	return n
}

func TestPlanMacNetwork(t *testing.T) {
	linux := Node{Metadata: ObjectMeta{Name: "linux-1", Labels: map[string]string{}}}
	linux.Status.Addresses = []NodeAddress{{Type: "InternalIP", Address: "10.0.0.9"}}
	nodes := []Node{
		macNode("studio-2", "tb-a", "169.254.1.2", "10.0.0.2"),
		macNode("studio-1", "tb-a", "169.254.1.1", "10.0.0.1"),
		macNode("mini-1", "", "", "10.0.0.3"),
		macNode("studio-3", "tb-a", "", "10.0.0.4"),
		linux,
	}
	net := MacNetwork{Metadata: ObjectMeta{Name: "shard", Namespace: "ai"}, Spec: MacNetworkSpec{Fabric: "tb-a", HostsPerNode: 10}}
	st := PlanMacNetwork(net, nodes, map[int]bool{192: true})
	if st.Phase != MacNetworkPhaseReady || st.SubnetIndex != 193 || st.Subnet != "10.89.193.0/24" || st.Port != 48193 || st.MTU != 1400 {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Members) != 2 || st.Members[0].Node != "studio-1" || st.Members[0].Hosts != [2]int{2, 11} || st.Members[0].Address != "169.254.1.1" || st.Members[0].Underlay != "thunderbolt" ||
		st.Members[1].Hosts != [2]int{12, 21} {
		t.Fatalf("members = %+v", st.Members)
	}
	if !strings.Contains(st.Message, "studio-3") {
		t.Fatalf("message = %q", st.Message)
	}

	// studio-1 leaves; studio-2 keeps its slice, a new Mac takes the free one.
	net.Status = st
	nodes = []Node{nodes[0], macNode("studio-4", "tb-a", "169.254.1.4", "")}
	st = PlanMacNetwork(net, nodes, map[int]bool{192: true})
	if st.SubnetIndex != 193 || len(st.Members) != 2 || st.Members[0].Node != "studio-2" || st.Members[0].Hosts != [2]int{12, 21} || st.Members[1].Hosts != [2]int{2, 11} {
		t.Fatalf("after change = %+v", st.Members)
	}

	// Rotation keeps the previous epoch.
	net.Status = st
	net.Spec.KeyEpoch = 1
	st = PlanMacNetwork(net, nodes, nil)
	if st.KeyEpoch != 1 || st.PreviousKeyEpoch == nil || *st.PreviousKeyEpoch != 0 {
		t.Fatalf("rotation = %+v", st)
	}

	// No fabric: every Mac over its InternalIP.
	all := PlanMacNetwork(MacNetwork{Metadata: ObjectMeta{Name: "lan", Namespace: "ai"}}, []Node{macNode("mini-1", "", "", "10.0.0.3"), linux}, nil)
	if len(all.Members) != 1 || all.Members[0].Address != "10.0.0.3" || all.Members[0].Underlay != "internal" || all.Members[0].Hosts != [2]int{2, 31} {
		t.Fatalf("lan = %+v", all)
	}
	full := map[int]bool{}
	for i := MacNetworkSubnetFirst; i <= MacNetworkSubnetLast; i++ {
		full[i] = true
	}
	if st := PlanMacNetwork(MacNetwork{Metadata: ObjectMeta{Name: "x"}}, nil, full); st.Phase != MacNetworkPhasePending || st.SubnetIndex != 0 {
		t.Fatalf("exhausted = %+v", st)
	}
}
