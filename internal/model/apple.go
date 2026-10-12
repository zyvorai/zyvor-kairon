// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Labels and annotations kairon-node publishes on a Mac Node.
const (
	LabelBackendVZ = "kairon.zyvor.dev/backend.vz"
	// TaintVMOnly keeps ordinary pods off a Mac: it has no kubelet. Machines
	// on the vz backend tolerate it without saying so.
	TaintVMOnly = "kairon.zyvor.dev/vm-only"

	LabelAppleChip           = "kairon.zyvor.dev/apple-chip"
	LabelAppleChipGeneration = "kairon.zyvor.dev/apple-chip-generation"
	LabelMacOSVersion        = "kairon.zyvor.dev/macos-version"
	LabelVZRosetta           = "kairon.zyvor.dev/vz.rosetta"
	LabelVZNested            = "kairon.zyvor.dev/vz.nested-virtualization"
	LabelThunderbolt         = "kairon.zyvor.dev/thunderbolt"
	LabelThunderboltFabric   = "kairon.zyvor.dev/thunderbolt-fabric"

	AnnotationAppleModel           = "kairon.zyvor.dev/apple-model"
	AnnotationAppleCores           = "kairon.zyvor.dev/apple-cores"
	AnnotationThunderboltInterface = "kairon.zyvor.dev/thunderbolt-interface"
	AnnotationThunderboltAddresses = "kairon.zyvor.dev/thunderbolt-addresses"
)

// AppleSpec asks for Apple silicon features. vz backend only.
type AppleSpec struct {
	// MinChipGeneration is the lowest Apple silicon generation: 3 is M3.
	MinChipGeneration int `json:"minChipGeneration,omitempty"`
	// MinMacOSVersion is the lowest macOS major version on the Mac.
	MinMacOSVersion int `json:"minMacOSVersion,omitempty"`
	// Rosetta exposes the Mac's Rosetta runtime to the Linux guest.
	Rosetta bool `json:"rosetta,omitempty"`
	// NestedVirtualization enables KVM inside the guest (M3 and macOS 15 or later).
	NestedVirtualization bool `json:"nestedVirtualization,omitempty"`
	// Networks are MacNetworks in the Machine's namespace; each adds a card
	// on that private network.
	Networks []string `json:"networks,omitempty"`
}

// Validate checks the spec on its own; backend checks happen where the
// backend is known.
func (a *AppleSpec) Validate() error {
	if a == nil {
		return nil
	}
	if a.MinChipGeneration < 0 || a.MinMacOSVersion < 0 {
		return fmt.Errorf("spec.apple: minimums cannot be negative")
	}
	seen := map[string]bool{}
	for _, n := range a.Networks {
		if n == "" || seen[n] {
			return fmt.Errorf("spec.apple.networks: %q is empty or listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// AppleNodeReason says why a Node cannot host a Machine with a, or "".
func AppleNodeReason(a *AppleSpec, labels map[string]string) string {
	if a == nil {
		return ""
	}
	if a.MinChipGeneration > 0 {
		g, err := strconv.Atoi(labels[LabelAppleChipGeneration])
		if err != nil || g < a.MinChipGeneration {
			return fmt.Sprintf("spec.apple.minChipGeneration %d, node chip %q", a.MinChipGeneration, labels[LabelAppleChip])
		}
	}
	if a.MinMacOSVersion > 0 {
		v, err := strconv.Atoi(labels[LabelMacOSVersion])
		if err != nil || v < a.MinMacOSVersion {
			return fmt.Sprintf("spec.apple.minMacOSVersion %d, node runs macOS %q", a.MinMacOSVersion, labels[LabelMacOSVersion])
		}
	}
	if a.Rosetta && labels[LabelVZRosetta] != "true" {
		return "spec.apple.rosetta, node has no Rosetta for Linux guests"
	}
	if a.NestedVirtualization && labels[LabelVZNested] != "true" {
		return "spec.apple.nestedVirtualization, node does not support it"
	}
	return ""
}

var appleChipRE = regexp.MustCompile(`^Apple M(\d+)(?:\s+(Pro|Max|Ultra))?$`)

// ParseAppleChip turns machdep.cpu.brand_string ("Apple M4 Max") into a
// label value ("m4-max") and the generation (4). ok is false for anything
// else (an Intel Mac, a virtual machine).
func ParseAppleChip(brand string) (label string, generation int, ok bool) {
	m := appleChipRE.FindStringSubmatch(strings.TrimSpace(brand))
	if m == nil {
		return "", 0, false
	}
	g, _ := strconv.Atoi(m[1])
	label = "m" + m[1]
	if m[2] != "" {
		label += "-" + strings.ToLower(m[2])
	}
	return label, g, true
}

// MacOSMajor is the major version of "27.2" or "15.6.1"; "" when unknown.
func MacOSMajor(version string) string {
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	if _, err := strconv.Atoi(major); err != nil {
		return ""
	}
	return major
}

// MacNetwork is a private Layer-2 network for vz Machines that spans Macs:
// FluxVM's switches on the member Macs form an encrypted mesh, over the
// Macs' Thunderbolt addresses when spec.fabric is set.
type MacNetwork struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta       `json:"metadata"`
	Spec     MacNetworkSpec   `json:"spec"`
	Status   MacNetworkStatus `json:"status,omitempty"`
}

type MacNetworkList struct {
	Items []MacNetwork `json:"items"`
}

func (n MacNetwork) Namespace() string {
	if n.Metadata.Namespace == "" {
		return "default"
	}
	return n.Metadata.Namespace
}

type MacNetworkSpec struct {
	// Fabric limits the network to Macs labelled
	// kairon.zyvor.dev/thunderbolt-fabric=<fabric> and carries it over their
	// Thunderbolt addresses. Empty: every Mac, over its InternalIP.
	Fabric string `json:"fabric,omitempty"`
	// MTU of the network, 1280-9000 (default 1400).
	MTU int `json:"mtu,omitempty"`
	// HostsPerNode is each Mac's slice of guest addresses (default 30).
	HostsPerNode int `json:"hostsPerNode,omitempty"`
	// KeyEpoch selects the network key; raising it rotates the key.
	KeyEpoch int `json:"keyEpoch,omitempty"`
}

type MacNetworkStatus struct {
	Phase string `json:"phase,omitempty"`
	// Name is the network's name on the Macs.
	Name string `json:"name,omitempty"`
	// Subnet is 10.89.N.0/24; SubnetIndex is N.
	Subnet      string `json:"subnet,omitempty"`
	SubnetIndex int    `json:"subnetIndex,omitempty"`
	Port        int    `json:"port,omitempty"`
	MTU         int    `json:"mtu,omitempty"`
	KeyEpoch    int    `json:"keyEpoch,omitempty"`
	// PreviousKeyEpoch is still accepted while the Macs roll to KeyEpoch.
	PreviousKeyEpoch *int               `json:"previousKeyEpoch,omitempty"`
	Members          []MacNetworkMember `json:"members,omitempty"`
	Message          string             `json:"message,omitempty"`
}

// MacNetworkMember is one Mac in the network.
type MacNetworkMember struct {
	Node string `json:"node"`
	// Hosts is the Mac's slice of host numbers, inclusive.
	Hosts [2]int `json:"hosts"`
	// Address is where the other Macs reach this Mac's switch.
	Address string `json:"address,omitempty"`
	// Underlay is thunderbolt or internal.
	Underlay string `json:"underlay,omitempty"`
}

// Member returns node's entry.
func (s MacNetworkStatus) Member(node string) (MacNetworkMember, bool) {
	for _, m := range s.Members {
		if m.Node == node {
			return m, true
		}
	}
	return MacNetworkMember{}, false
}

const (
	MacNetworkPhaseReady   = "Ready"
	MacNetworkPhasePending = "Pending"

	// Kairon allocates 10.89.192.0/24 to 10.89.254.0/24, above the range a
	// FluxVM fleet central uses, and UDP port 48000+N.
	MacNetworkSubnetFirst = 192
	MacNetworkSubnetLast  = 254
	MacNetworkPortBase    = 48000

	DefaultMacNetworkMTU          = 1400
	DefaultMacNetworkHostsPerNode = 30
)

// EffectiveMTU applies the default.
func (s MacNetworkSpec) EffectiveMTU() int {
	if s.MTU == 0 {
		return DefaultMacNetworkMTU
	}
	return s.MTU
}

// EffectiveHostsPerNode applies the default.
func (s MacNetworkSpec) EffectiveHostsPerNode() int {
	if s.HostsPerNode == 0 {
		return DefaultMacNetworkHostsPerNode
	}
	return s.HostsPerNode
}

func (s MacNetworkSpec) Validate() error {
	if m := s.EffectiveMTU(); m < 1280 || m > 9000 {
		return fmt.Errorf("spec.mtu must be 1280-9000, got %d", m)
	}
	if h := s.EffectiveHostsPerNode(); h < 1 || h > 253 {
		return fmt.Errorf("spec.hostsPerNode must be 1-253, got %d", h)
	}
	if s.KeyEpoch < 0 {
		return fmt.Errorf("spec.keyEpoch cannot be negative")
	}
	return nil
}

// macNetworkAddress is where the other Macs reach node's switch, and over
// what: its first Thunderbolt address for a fabric network, else its
// InternalIP.
func macNetworkAddress(n Node, fabric string) (addr, underlay string) {
	if fabric != "" {
		for _, a := range strings.Split(n.Metadata.Annotations[AnnotationThunderboltAddresses], ",") {
			if a = strings.TrimSpace(a); a != "" {
				return a, "thunderbolt"
			}
		}
		return "", ""
	}
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" && a.Address != "" {
			return a.Address, "internal"
		}
	}
	return "", ""
}

// PlanMacNetwork is net's status for nodes. taken holds the subnet indexes
// other MacNetworks use. Members keep their slices; new ones get the lowest
// free slice.
func PlanMacNetwork(net MacNetwork, nodes []Node, taken map[int]bool) MacNetworkStatus {
	st := net.Status
	st.Members = nil
	st.Message = ""
	st.Name = MacNetworkName(net.Namespace(), net.Metadata.Name)
	st.MTU = net.Spec.EffectiveMTU()
	if err := net.Spec.Validate(); err != nil {
		st.Phase, st.Message = MacNetworkPhasePending, err.Error()
		return st
	}
	if st.SubnetIndex < MacNetworkSubnetFirst || st.SubnetIndex > MacNetworkSubnetLast || taken[st.SubnetIndex] {
		st.SubnetIndex = 0
		for i := MacNetworkSubnetFirst; i <= MacNetworkSubnetLast; i++ {
			if !taken[i] {
				st.SubnetIndex = i
				break
			}
		}
		if st.SubnetIndex == 0 {
			st.Phase, st.Subnet, st.Port = MacNetworkPhasePending, "", 0
			st.Message = "no free subnet: Kairon has 10.89.192.0/24 to 10.89.254.0/24"
			return st
		}
	}
	st.Subnet = fmt.Sprintf("10.89.%d.0/24", st.SubnetIndex)
	st.Port = MacNetworkPortBase + st.SubnetIndex
	if st.KeyEpoch != net.Spec.KeyEpoch {
		if net.Status.Name != "" {
			prev := st.KeyEpoch
			st.PreviousKeyEpoch = &prev
		}
		st.KeyEpoch = net.Spec.KeyEpoch
	}

	size := net.Spec.EffectiveHostsPerNode()
	used := map[int]bool{}
	var keep, fresh []MacNetworkMember
	var notes []string
	sorted := append([]Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Metadata.Name < sorted[j].Metadata.Name })
	for _, n := range sorted {
		if n.Metadata.Labels[LabelBackendVZ] != "true" || (net.Spec.Fabric != "" && n.Metadata.Labels[LabelThunderboltFabric] != net.Spec.Fabric) {
			continue
		}
		addr, underlay := macNetworkAddress(n, net.Spec.Fabric)
		if addr == "" {
			notes = append(notes, n.Metadata.Name+" has no address to reach")
			continue
		}
		m := MacNetworkMember{Node: n.Metadata.Name, Address: addr, Underlay: underlay}
		if old, ok := net.Status.Member(n.Metadata.Name); ok && old.Hosts[0] >= 2 && old.Hosts[1]-old.Hosts[0]+1 == size && old.Hosts[1] <= 254 {
			m.Hosts = old.Hosts
			for h := old.Hosts[0]; h <= old.Hosts[1]; h++ {
				used[h] = true
			}
			keep = append(keep, m)
			continue
		}
		fresh = append(fresh, m)
	}
	for _, m := range fresh {
		lo := 0
		for start := 2; start+size-1 <= 254; start += size {
			free := true
			for h := start; h < start+size; h++ {
				free = free && !used[h]
			}
			if free {
				lo = start
				break
			}
		}
		if lo == 0 {
			notes = append(notes, m.Node+" has no free address slice")
			continue
		}
		m.Hosts = [2]int{lo, lo + size - 1}
		for h := lo; h < lo+size; h++ {
			used[h] = true
		}
		keep = append(keep, m)
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].Node < keep[j].Node })
	st.Members = keep
	st.Phase = MacNetworkPhaseReady
	if len(keep) == 0 {
		st.Phase = MacNetworkPhasePending
		notes = append([]string{"no Mac matches"}, notes...)
	}
	st.Message = strings.Join(notes, "; ")
	return st
}

// MacNetworkName is the network's name on the Macs: FluxVM allows a-z, 0-9
// and -, at most 40 characters, and one namespace's name must not collide
// with another's.
func MacNetworkName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	short := strings.Trim(strings.ReplaceAll(strings.ToLower(name), ".", "-"), "-")
	if len(short) > 28 {
		short = strings.TrimRight(short[:28], "-")
	}
	if short == "" {
		return "k" + hex.EncodeToString(sum[:4])
	}
	return "k" + hex.EncodeToString(sum[:4]) + "-" + short
}
