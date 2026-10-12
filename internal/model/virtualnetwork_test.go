// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

func TestVirtualNetworkPrefixAndGateway(t *testing.T) {
	for _, bad := range []string{"", "10.0.0.0", "fd00::/64", "10.0.0.1/24", "10.0.0.0/7", "10.0.0.0/30"} {
		if _, err := VirtualNetworkPrefix(bad); err == nil {
			t.Errorf("VirtualNetworkPrefix(%q) accepted", bad)
		}
	}
	gw, err := VirtualNetworkSpec{CIDR: "10.50.0.0/24"}.EffectiveGateway()
	if err != nil || gw.String() != "10.50.0.1" {
		t.Fatalf("default gateway = %v, %v", gw, err)
	}
	if gw, err := (VirtualNetworkSpec{CIDR: "10.50.0.0/24", Gateway: "10.50.0.254"}).EffectiveGateway(); err != nil || gw.String() != "10.50.0.254" {
		t.Fatalf("explicit gateway = %v, %v", gw, err)
	}
	for _, bad := range []string{"10.50.0.0", "10.50.0.255", "10.51.0.1", "fd00::1"} {
		if _, err := (VirtualNetworkSpec{CIDR: "10.50.0.0/24", Gateway: bad}).EffectiveGateway(); err == nil {
			t.Errorf("gateway %q accepted", bad)
		}
	}
	if err := ValidateVirtualNetworkSpec(VirtualNetworkSpec{CIDR: "10.50.0.0/24", VNI: VNIMax + 1}); err == nil {
		t.Fatal("VNI above 24 bits accepted")
	}
}

func TestValidateMachineVirtualNetwork(t *testing.T) {
	ok := NetworkSpec{VirtualNetwork: &MachineVirtualNetwork{Name: "app", IP: "10.50.0.20", FloatingIP: "203.0.113.10"}}
	if err := ValidateMachineVirtualNetwork(ok); err != nil {
		t.Fatal(err)
	}
	// The CRD defaults an unset mode to user.
	ok.Mode = "user"
	if err := ValidateMachineVirtualNetwork(ok); err != nil {
		t.Fatalf("defaulted mode: %v", err)
	}
	cases := map[string]NetworkSpec{
		"name":       {VirtualNetwork: &MachineVirtualNetwork{}},
		"mode":       {Mode: "macvtap", VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"netns":      {Mode: "tap", NetNS: true, VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"bridge":     {Mode: "tap", Bridge: "br0", VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"extra":      {ExtraInterfaces: []ExtraInterface{{Name: "x", Bridge: "b"}}, VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"cilium":     {CiliumAttach: true, VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"legacy":     {DataplaneMode: "legacy", VirtualNetwork: &MachineVirtualNetwork{Name: "a"}},
		"ip":         {VirtualNetwork: &MachineVirtualNetwork{Name: "a", IP: "fd00::5"}},
		"floatingIP": {VirtualNetwork: &MachineVirtualNetwork{Name: "a", FloatingIP: "nope"}},
	}
	for name, n := range cases {
		if err := ValidateMachineVirtualNetwork(n); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEffectiveNetworkVirtualNetwork(t *testing.T) {
	m := Machine{Metadata: ObjectMeta{Name: "web", Namespace: "prod"}, Spec: MachineSpec{Network: NetworkSpec{VirtualNetwork: &MachineVirtualNetwork{Name: "app"}}}}
	n := m.EffectiveNetwork()
	if n.Mode != "tap" || n.MAC != DefaultMAC("prod", "web") {
		t.Fatalf("effective network = %+v", n)
	}
}

func TestAllocateVirtualNetwork(t *testing.T) {
	spec := VirtualNetworkSpec{CIDR: "10.50.0.0/29"} // hosts .1-.6, gateway .1
	members := []VirtualNetworkMember{
		{Machine: "b", MAC: "m-b"},
		{Machine: "a", MAC: "m-a", IP: "10.50.0.2"},
		{Machine: "c", MAC: "m-c", Node: "n1", NodeIP: "192.0.2.1"},
	}
	allocs, refused, err := AllocateVirtualNetwork(spec, nil, members)
	if err != nil || len(refused) != 0 {
		t.Fatalf("err=%v refused=%v", err, refused)
	}
	got := map[string]string{}
	for _, a := range allocs {
		got[a.Machine] = a.IP
	}
	// The explicit request is honored before b and c take the lowest free.
	if got["a"] != "10.50.0.2" || got["b"] != "10.50.0.3" || got["c"] != "10.50.0.4" {
		t.Fatalf("allocations = %v", got)
	}
	if allocs[2].Node != "n1" || allocs[2].NodeIP != "192.0.2.1" || allocs[2].MAC != "m-c" {
		t.Fatalf("placement not carried: %+v", allocs[2])
	}

	// Sticky: b keeps .3 after a leaves and d joins, d gets .2 back.
	members = make([]VirtualNetworkMember, 0, 9)
	members = append(members, VirtualNetworkMember{Machine: "b", MAC: "m-b"}, VirtualNetworkMember{Machine: "c", MAC: "m-c"}, VirtualNetworkMember{Machine: "d", MAC: "m-d"})
	allocs, refused, err = AllocateVirtualNetwork(spec, allocs, members)
	if err != nil || len(refused) != 0 {
		t.Fatalf("err=%v refused=%v", err, refused)
	}
	got = map[string]string{}
	for _, a := range allocs {
		got[a.Machine] = a.IP
	}
	if got["b"] != "10.50.0.3" || got["c"] != "10.50.0.4" || got["d"] != "10.50.0.2" || got["a"] != "" {
		t.Fatalf("sticky allocations = %v", got)
	}

	// Refusals: gateway, out of range, taken, and exhaustion.
	members = append(members,
		VirtualNetworkMember{Machine: "gw", IP: "10.50.0.1"},
		VirtualNetworkMember{Machine: "out", IP: "10.60.0.1"},
		VirtualNetworkMember{Machine: "taken", IP: "10.50.0.3"},
		VirtualNetworkMember{Machine: "e"}, VirtualNetworkMember{Machine: "f"}, VirtualNetworkMember{Machine: "g"},
	)
	allocs, refused, err = AllocateVirtualNetwork(spec, allocs, members)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refused["gw"], "gateway") || !strings.Contains(refused["out"], "not a host address") || !strings.Contains(refused["taken"], "Machine b") || !strings.Contains(refused["g"], "no free address") {
		t.Fatalf("refused = %v", refused)
	}
	if len(allocs) != 5 {
		t.Fatalf("allocations = %+v", allocs)
	}
}

func TestAllocateVNI(t *testing.T) {
	v, err := AllocateVNI(map[uint32]bool{VNIAutoMin: true, VNIAutoMin + 1: true})
	if err != nil || v != VNIAutoMin+2 {
		t.Fatalf("AllocateVNI = %d, %v", v, err)
	}
}
