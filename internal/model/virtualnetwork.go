// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const KindVirtualNetwork = "VirtualNetwork"

// VNI bounds. VNIs below VNIAutoMin are left for spec.vni so an operator can
// pin well-known networks without colliding with allocated ones.
const (
	VNIMax     = 1<<24 - 1
	VNIAutoMin = 4096
)

// VirtualNetwork is an isolated IPv4 network for Machines: one VNI, one CIDR,
// a distributed gateway on every node and optional egress SNAT. FluxVM routes
// it with eBPF and carries it between nodes in Geneve; kairon-controller is its
// IPAM (status.allocations) and kairon-node pushes it to FluxVM. See
// docs/guides/virtual-networks.md.
type VirtualNetwork struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta           `json:"metadata"`
	Spec     VirtualNetworkSpec   `json:"spec"`
	Status   VirtualNetworkStatus `json:"status,omitempty"`
}

func (v VirtualNetwork) Namespace() string { return v.Metadata.Namespace }

type VirtualNetworkList struct {
	TypeMeta `json:",inline"`
	Items    []VirtualNetwork `json:"items"`
}

type VirtualNetworkSpec struct {
	// CIDR is the network's IPv4 prefix, /8 to /29.
	CIDR string `json:"cidr"`
	// Gateway defaults to the first host address of CIDR.
	Gateway string `json:"gateway,omitempty"`
	// VNI pins the network identifier (1..16777215); empty allocates one at
	// or above 4096. It cannot change once allocated.
	VNI uint32 `json:"vni,omitempty"`
	// EgressSNAT lets Machines reach addresses outside CIDR, translated to
	// the node's address on its uplink. Off: the network is closed.
	EgressSNAT bool `json:"egressSNAT,omitempty"`
}

type VirtualNetworkStatus struct {
	Phase   string `json:"phase,omitempty"` // Ready|Failed
	Message string `json:"message"`
	VNI     uint32 `json:"vni,omitempty"`
	Gateway string `json:"gateway,omitempty"`
	// Allocations is the network's IPAM: one entry per attached Machine,
	// kept for the Machine's lifetime and released when it is deleted or
	// stops referencing the network.
	// No omitempty: the status merge patch must be able to clear the list.
	Allocations        []VirtualNetworkAllocation `json:"allocations"`
	ObservedGeneration int64                      `json:"observedGeneration,omitempty"`
}

type VirtualNetworkAllocation struct {
	Machine    string `json:"machine"`
	IP         string `json:"ip"`
	MAC        string `json:"mac"`
	FloatingIP string `json:"floatingIP,omitempty"`
	// Node and NodeIP place the Machine for the overlay; empty until it is
	// scheduled.
	Node   string `json:"node,omitempty"`
	NodeIP string `json:"nodeIP,omitempty"`
}

// MachineVirtualNetwork attaches a Machine's primary NIC to a VirtualNetwork
// in the Machine's namespace.
type MachineVirtualNetwork struct {
	Name string `json:"name"`
	// IP requests a specific address; empty takes the lowest free one.
	IP string `json:"ip,omitempty"`
	// FloatingIP is a public IPv4 address translated 1:1 to the Machine.
	// It must be routed to the Machine's node (kairon-node's BGP speaker
	// announces it, or a static route).
	FloatingIP string `json:"floatingIP,omitempty"`
}

// MachineVirtualNetworkStatus is the attachment as kairon-node created it.
type MachineVirtualNetworkStatus struct {
	Name         string `json:"name"`
	VNI          uint32 `json:"vni"`
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefixLength"`
	Gateway      string `json:"gateway"`
	MAC          string `json:"mac"`
	FloatingIP   string `json:"floatingIP,omitempty"`
}

// VirtualNetworkPrefix parses and checks spec.cidr.
func VirtualNetworkPrefix(cidr string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("spec.cidr %q is not an IPv4 CIDR", cidr)
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("spec.cidr %q has host bits set (use %s)", cidr, p.Masked())
	}
	if p.Bits() < 8 || p.Bits() > 29 {
		return netip.Prefix{}, fmt.Errorf("spec.cidr %q: prefix length must be 8..29", cidr)
	}
	return p, nil
}

// EffectiveGateway is spec.gateway or the first host address of the CIDR.
func (s VirtualNetworkSpec) EffectiveGateway() (netip.Addr, error) {
	p, err := VirtualNetworkPrefix(s.CIDR)
	if err != nil {
		return netip.Addr{}, err
	}
	if s.Gateway == "" {
		return p.Addr().Next(), nil
	}
	gw, err := netip.ParseAddr(s.Gateway)
	if err != nil || !gw.Is4() || !p.Contains(gw) || gw == p.Addr() || gw == lastAddr(p) {
		return netip.Addr{}, fmt.Errorf("spec.gateway %q must be a host address inside %s", s.Gateway, p)
	}
	return gw, nil
}

// ValidateVirtualNetworkSpec checks a VirtualNetwork spec on its own.
func ValidateVirtualNetworkSpec(s VirtualNetworkSpec) error {
	if _, err := s.EffectiveGateway(); err != nil {
		return err
	}
	if s.VNI > VNIMax {
		return fmt.Errorf("spec.vni %d is above %d", s.VNI, VNIMax)
	}
	return nil
}

// ValidateMachineVirtualNetwork checks spec.network.virtualNetwork against
// the rest of the network spec: a vnet NIC is a bridge-less tap that the
// virtual network alone routes.
func ValidateMachineVirtualNetwork(n NetworkSpec) error {
	v := n.VirtualNetwork
	if v == nil {
		return nil
	}
	if v.Name == "" {
		return fmt.Errorf("spec.network.virtualNetwork.name is required")
	}
	// "user" is the CRD default for an unset mode; the virtual network
	// turns the NIC into a tap either way.
	if n.Mode == "macvtap" {
		return fmt.Errorf("spec.network.virtualNetwork needs spec.network.mode tap (got %q)", n.Mode)
	}
	switch {
	case n.NetNS:
		return fmt.Errorf("spec.network.virtualNetwork cannot be combined with netns")
	case n.Bridge != "":
		return fmt.Errorf("spec.network.virtualNetwork cannot be combined with a bridge")
	case len(n.ExtraInterfaces) > 0:
		return fmt.Errorf("spec.network.virtualNetwork cannot be combined with extraInterfaces")
	case n.CiliumAttach:
		return fmt.Errorf("spec.network.virtualNetwork cannot be combined with ciliumAttach")
	case strings.EqualFold(n.DataplaneMode, "legacy"):
		return fmt.Errorf("spec.network.virtualNetwork needs the eBPF dataplane (dataplaneMode legacy routes nothing)")
	}
	for field, s := range map[string]string{"ip": v.IP, "floatingIP": v.FloatingIP} {
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return fmt.Errorf("spec.network.virtualNetwork.%s %q is not an IPv4 address", field, s)
		}
	}
	return nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := (uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// VirtualNetworkMember is one Machine that references a network, as the IPAM
// sees it.
type VirtualNetworkMember struct {
	Machine    string
	MAC        string
	IP         string // requested, may be empty
	FloatingIP string
	Node       string
	NodeIP     string
}

// AllocateVirtualNetwork computes a network's allocations: existing ones are
// kept (with fresh placement), new members get their requested address when
// free or the lowest free one, departed members are released. Members whose
// request cannot be met are returned with the reason, keyed by Machine name.
func AllocateVirtualNetwork(spec VirtualNetworkSpec, prev []VirtualNetworkAllocation, members []VirtualNetworkMember) ([]VirtualNetworkAllocation, map[string]string, error) {
	p, err := VirtualNetworkPrefix(spec.CIDR)
	if err != nil {
		return nil, nil, err
	}
	gw, err := spec.EffectiveGateway()
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]VirtualNetworkMember{}
	for _, m := range members {
		byName[m.Machine] = m
	}
	used := map[netip.Addr]string{}
	out := make([]VirtualNetworkAllocation, 0, len(members))
	kept := map[string]bool{}
	for _, a := range prev {
		m, ok := byName[a.Machine]
		if !ok {
			continue
		}
		ip, err := netip.ParseAddr(a.IP)
		// An allocation that no longer fits the network, or no longer
		// matches an explicit request, is redone below.
		if err != nil || !p.Contains(ip) || ip == gw || (m.IP != "" && m.IP != a.IP) || used[ip] != "" {
			continue
		}
		used[ip] = a.Machine
		kept[a.Machine] = true
		out = append(out, VirtualNetworkAllocation{Machine: a.Machine, IP: a.IP, MAC: m.MAC, FloatingIP: m.FloatingIP, Node: m.Node, NodeIP: m.NodeIP})
	}
	refused := map[string]string{}
	names := make([]string, 0, len(members))
	for _, m := range members {
		if !kept[m.Machine] {
			names = append(names, m.Machine)
		}
	}
	sort.Strings(names)
	// Explicit requests first, so a lowest-free pick never takes a requested address.
	sort.SliceStable(names, func(i, j int) bool { return byName[names[i]].IP != "" && byName[names[j]].IP == "" })
	for _, name := range names {
		m := byName[name]
		var ip netip.Addr
		if m.IP != "" {
			want, err := netip.ParseAddr(m.IP)
			switch {
			case err != nil || !p.Contains(want) || want == p.Addr() || want == lastAddr(p):
				refused[name] = fmt.Sprintf("requested address %s is not a host address of %s", m.IP, p)
				continue
			case want == gw:
				refused[name] = fmt.Sprintf("requested address %s is the gateway", m.IP)
				continue
			case used[want] != "":
				refused[name] = fmt.Sprintf("requested address %s is allocated to Machine %s", m.IP, used[want])
				continue
			}
			ip = want
		} else {
			for a := p.Addr().Next(); p.Contains(a) && a != lastAddr(p); a = a.Next() {
				if a != gw && used[a] == "" {
					ip = a
					break
				}
			}
			if !ip.IsValid() {
				refused[name] = fmt.Sprintf("no free address left in %s", p)
				continue
			}
		}
		used[ip] = name
		out = append(out, VirtualNetworkAllocation{Machine: name, IP: ip.String(), MAC: m.MAC, FloatingIP: m.FloatingIP, Node: m.Node, NodeIP: m.NodeIP})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out, refused, nil
}

// AllocateVNI picks the lowest VNI at or above VNIAutoMin not in used.
func AllocateVNI(used map[uint32]bool) (uint32, error) {
	for v := uint32(VNIAutoMin); v <= VNIMax; v++ {
		if !used[v] {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no free VNI")
}

// NodeInternalIP is the node's first InternalIP address, or "".
func NodeInternalIP(n Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == "InternalIP" && strings.TrimSpace(a.Address) != "" {
			return strings.TrimSpace(a.Address)
		}
	}
	return ""
}

// AllocationFor returns the allocation of machine, if any.
func (s VirtualNetworkStatus) AllocationFor(machine string) (VirtualNetworkAllocation, bool) {
	for _, a := range s.Allocations {
		if a.Machine == machine {
			return a, true
		}
	}
	return VirtualNetworkAllocation{}, false
}
