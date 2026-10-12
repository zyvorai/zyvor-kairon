// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	KindMachineNetworkPolicy  = "MachineNetworkPolicy"
	KindNetworkSecurityGroup  = "NetworkSecurityGroup"
	FinalizerNetworkPolicy    = "kairon.zyvor.dev/network-policy"
	FinalizerNetworkGroup     = "kairon.zyvor.dev/network-group"
	FinalizerCiliumAttach     = "kairon.zyvor.dev/cilium-attach"
	FinalizerCiliumPolicySync = "kairon.zyvor.dev/cilium-policy-sync"
	LabelMachineNamespace     = "kairon.zyvor.dev/machine-namespace"
	LabelMachineName          = "kairon.zyvor.dev/machine-name"
	LabelManagedBy            = "app.kubernetes.io/managed-by"
	ManagedByKairon           = "kairon"
)

// ValidateCiliumAttach checks prerequisites for joining the Cilium cluster
// network via ExternalWorkload: mode=tap and netns=true.
func ValidateCiliumAttach(n NetworkSpec) error {
	if !n.CiliumAttach {
		return nil
	}
	mode := strings.ToLower(n.Mode)
	if mode != "tap" {
		return fmt.Errorf("spec.network.ciliumAttach requires mode=tap (got %q)", n.Mode)
	}
	if !n.NetNS {
		return fmt.Errorf("spec.network.ciliumAttach requires netns=true")
	}
	switch strings.ToLower(n.DataplaneMode) {
	case "", "legacy", "ebpf", "cilium":
	default:
		return fmt.Errorf("spec.network.dataplaneMode %q is invalid; use legacy, ebpf, or cilium", n.DataplaneMode)
	}
	return nil
}

// ValidateDataplaneMode rejects unknown dataplaneMode values when set.
func ValidateDataplaneMode(n NetworkSpec) error {
	switch strings.ToLower(n.DataplaneMode) {
	case "", "legacy", "ebpf", "cilium":
		return nil
	default:
		return fmt.Errorf("spec.network.dataplaneMode %q is invalid; use legacy, ebpf, or cilium", n.DataplaneMode)
	}
}

// PortForward maps FluxVM user-mode NAT forwards (host↔guest).
type PortForward struct {
	HostPort  uint16 `json:"hostPort"`
	GuestPort uint16 `json:"guestPort"`
	Protocol  string `json:"protocol,omitempty"` // tcp|udp; default tcp
}

// NetworkSpec is Machine.create parity with Fabric / FluxVM NetworkSpec.
type NetworkSpec struct {
	Mode              string        `json:"mode,omitempty"` // user|tap|macvtap
	NetNS             bool          `json:"netns,omitempty"`
	Bridge            string        `json:"bridge,omitempty"`
	Parent            string        `json:"parent,omitempty"`
	MAC               string        `json:"mac,omitempty"`
	TapName           string        `json:"tapName,omitempty"`
	MacvtapMode       string        `json:"macvtapMode,omitempty"` // bridge|vepa|private|passthru
	Forwards          []PortForward `json:"forwards,omitempty"`
	StaticNetwork     bool          `json:"staticNetwork,omitempty"`
	PodUID            string        `json:"podUID,omitempty"`
	DataplaneRequired bool          `json:"dataplaneRequired,omitempty"`
	// DataplaneMode requests FluxVM sandbox dataplane: legacy|ebpf|cilium.
	// Empty leaves the host's sandbox.dataplane setting from fluxvm.toml;
	// current FluxVM defaults an omitted setting to mode=ebpf, required=true.
	// Attachment strictness (required) is always the host's, not per Machine.
	DataplaneMode string `json:"dataplaneMode,omitempty"`
	// CiliumAttach, when true, asks kairon-controller to reconcile a
	// CiliumExternalWorkload so the Machine can join the cluster Cilium
	// network (identity/IPAM). Requires mode=tap and netns=true.
	CiliumAttach bool `json:"ciliumAttach,omitempty"`
	// CiliumNamespace is reserved for future namespaced attach hints;
	// ExternalWorkload is cluster-scoped — labels carry Machine identity.
	CiliumNamespace string            `json:"ciliumNamespace,omitempty"`
	CiliumLabels    map[string]string `json:"ciliumLabels,omitempty"`
	// AntiSpoof drops frames whose source MAC or IP is not the one assigned
	// to this Machine. Enforced by the FluxVM eBPF edge, not by Kairon.
	AntiSpoof bool `json:"antiSpoof,omitempty"`
	// LearnIP projects status.network.guestIP from ARP, DHCP, or ND on the
	// tap so agent-less and Windows guests still get an address.
	LearnIP bool `json:"learnIP,omitempty"`
	// QoS is the per-Machine token bucket projected into the edge map.
	QoS *NetworkQoS `json:"qos,omitempty"`
	// ExtraInterfaces are bridged NICs hot-added to the running guest
	// (mode tap only) and hot-removed when dropped from this list.
	ExtraInterfaces []ExtraInterface `json:"extraInterfaces,omitempty"`
	// VirtualNetwork attaches the primary NIC to a VirtualNetwork in the
	// Machine's namespace (mode tap, no bridge, no netns).
	VirtualNetwork *MachineVirtualNetwork `json:"virtualNetwork,omitempty"`
}

// MaxExtraInterfaces is how many NICs fit next to the primary on FluxVM's
// four hotplug PCIe ports.
const MaxExtraInterfaces = 3

// ExtraInterface is one hot-pluggable bridged NIC.
type ExtraInterface struct {
	Name   string `json:"name"`
	Bridge string `json:"bridge"`
	// MAC defaults to one derived from the Machine UID and Name, so it is
	// stable across restarts.
	MAC string `json:"mac,omitempty"`
}

// ExtraInterfaceMAC is iface.MAC, or a locally administered unicast MAC
// derived from the Machine UID and interface name.
func ExtraInterfaceMAC(machineUID string, iface ExtraInterface) string {
	if iface.MAC != "" {
		return strings.ToLower(iface.MAC)
	}
	sum := sha256.Sum256([]byte(machineUID + "/" + iface.Name))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3], sum[4])
}

// ValidateExtraInterfaces checks spec.network.extraInterfaces.
func ValidateExtraInterfaces(ns NetworkSpec) error {
	if len(ns.ExtraInterfaces) == 0 {
		return nil
	}
	if ns.Mode != "tap" {
		return fmt.Errorf("spec.network.extraInterfaces needs spec.network.mode tap (got %q)", ns.Mode)
	}
	if len(ns.ExtraInterfaces) > MaxExtraInterfaces {
		return fmt.Errorf("spec.network.extraInterfaces: at most %d", MaxExtraInterfaces)
	}
	seen := map[string]bool{}
	for i, iface := range ns.ExtraInterfaces {
		if iface.Name == "" || iface.Bridge == "" {
			return fmt.Errorf("spec.network.extraInterfaces[%d]: name and bridge are required", i)
		}
		if seen[iface.Name] {
			return fmt.Errorf("spec.network.extraInterfaces[%d]: duplicate name %q", i, iface.Name)
		}
		seen[iface.Name] = true
		if iface.MAC != "" {
			hw, err := net.ParseMAC(iface.MAC)
			if err != nil || len(hw) != 6 {
				return fmt.Errorf("spec.network.extraInterfaces[%d].mac %q: not a 6-octet Ethernet address", i, iface.MAC)
			}
		}
	}
	return nil
}

// NetworkQoS is a token bucket. A nil pointer means no bucket. An explicit
// zero is rejected by ValidateNetworkQoS.
type NetworkQoS struct {
	IngressMbps *uint32 `json:"ingressMbps,omitempty"`
	EgressMbps  *uint32 `json:"egressMbps,omitempty"`
	IngressPps  *uint32 `json:"ingressPps,omitempty"`
	EgressPps   *uint32 `json:"egressPps,omitempty"`
}

// ServiceFabricSpec declares FluxVM Service Fabric VIP membership for a Machine.
type ServiceFabricSpec struct {
	Services []ServiceFabricMembership `json:"services,omitempty"`
}

// AppliedServiceFabricMembership records one Service Fabric backend entry
// Kairon has actually told FluxVM to register for a Machine -- the
// (name, port, guestIP) triple that was live at the time it was applied,
// not just the current spec.serviceFabric.services -- so a later reconcile
// can tell exactly which backend entries are now stale and must be
// deregistered: a membership removed from spec, or the same membership
// re-applied against a *different* guestIP (a DHCP re-lease, a guest
// reboot landing on a new address, ...) since it was last applied. Without
// keeping this ground truth in status, only guestIP as of *this* tick
// could ever be reasoned about, and a prior tick's now-superseded address
// would never be found again to remove. See
// internal/agent/network.go's reconcileServiceFabric and
// deregisterServiceFabric for how this is produced and consumed.
type AppliedServiceFabricMembership struct {
	Name    string `json:"name"`
	Port    uint16 `json:"port"`
	GuestIP string `json:"guestIP"`
}

// ServiceFabricMembership registers the Machine guest IP as a backend of a named service.
type ServiceFabricMembership struct {
	Name   string `json:"name"`
	Port   uint16 `json:"port"`
	Weight uint16 `json:"weight,omitempty"`
}

// MachineNetworkStatus is projected for Fabric's Dataplane tab.
type MachineNetworkStatus struct {
	GuestIP   string                  `json:"guestIP,omitempty"`
	GuestIPs  []string                `json:"guestIPs,omitempty"`
	TapName   string                  `json:"tapName,omitempty"`
	Dataplane *MachineDataplaneStatus `json:"dataplane,omitempty"`
	Cilium    *MachineCiliumStatus    `json:"cilium,omitempty"`
	// Edge is the VM-edge eBPF projection (identity, learn source, conntrack).
	Edge *MachineEdgeStatus `json:"edge,omitempty"`
	// VirtualNetwork is the spec.network.virtualNetwork attachment the
	// runtime was created with.
	VirtualNetwork *MachineVirtualNetworkStatus `json:"virtualNetwork,omitempty"`
}

// MachineEdgeStatus is the control-plane view of the FluxVM eBPF edge.
type MachineEdgeStatus struct {
	Identity          uint32 `json:"identity,omitempty"`
	GuestIPSource     string `json:"guestIPSource,omitempty"` // agent|arp|dhcp|nd
	AntiSpoof         bool   `json:"antiSpoof,omitempty"`
	PolicyName        string `json:"policyName,omitempty"`
	ConntrackRestored int    `json:"conntrackRestored,omitempty"`
	BlackholeWindowMs int64  `json:"blackholeWindowMs,omitempty"`
}

// MachineCiliumStatus is projected when ciliumAttach (or CNP sync) is in use.
type MachineCiliumStatus struct {
	ExternalWorkload    string `json:"externalWorkload,omitempty"`
	ExternalWorkloadUID string `json:"externalWorkloadUID,omitempty"`
	Identity            uint32 `json:"identity,omitempty"`
	IPv4                string `json:"ipv4,omitempty"`
	Message             string `json:"message,omitempty"`
}

// MachineDataplaneStatus mirrors FluxVM GET …/network/status fields Fabric expects.
type MachineDataplaneStatus struct {
	Attached          bool    `json:"attached"`
	Mode              string  `json:"mode,omitempty"`
	SchemaVersion     *uint32 `json:"schemaVersion,omitempty"`
	Identity          uint32  `json:"identity,omitempty"`
	PolicyFingerprint string  `json:"policyFingerprint,omitempty"`
	PolicySynced      bool    `json:"policySynced,omitempty"`
	Interface         string  `json:"interface,omitempty"`
}

// VmNetworkPolicy mirrors FluxVM VmNetworkPolicy (snake_case on the wire via fluxvm client).
type VmNetworkPolicy struct {
	DefaultAllow  bool     `json:"defaultAllow"`
	AllowCidrs    []string `json:"allowCidrs,omitempty"`
	DenyCidrs     []string `json:"denyCidrs,omitempty"`
	AllowPorts    []string `json:"allowPorts,omitempty"`
	MaxEgressMbps *uint32  `json:"maxEgressMbps,omitempty"`
	MaxEgressPps  *uint32  `json:"maxEgressPps,omitempty"`
	AllowFqdns    []string `json:"allowFqdns,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	Entities      []string `json:"entities,omitempty"`
	AuditMode     bool     `json:"auditMode,omitempty"`
	AllowIcmp     bool     `json:"allowIcmp,omitempty"`
	SampleRate    uint32   `json:"sampleRate,omitempty"`
	// AllowSNI is a TLS SNI allow list (exact or "*.suffix"). Not an HTTP proxy.
	AllowSNI []string `json:"allowSNI,omitempty"`
	// AllowDNS is a DNS qname allow list. Empty falls through to AllowFqdns.
	AllowDNS       []string `json:"allowDNS,omitempty"`
	MaxIngressMbps *uint32  `json:"maxIngressMbps,omitempty"`
	MaxIngressPps  *uint32  `json:"maxIngressPps,omitempty"`
}

// MachineNetworkPolicy selects Machines and applies FluxVM VM-edge policy.
type MachineNetworkPolicy struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                 `json:"metadata"`
	Spec     MachineNetworkPolicySpec   `json:"spec"`
	Status   MachineNetworkPolicyStatus `json:"status,omitempty"`
}

type MachineNetworkPolicyList struct {
	TypeMeta `json:",inline"`
	Items    []MachineNetworkPolicy `json:"items"`
}

type MachineNetworkPolicySpec struct {
	// MachineName targets a single Machine in the same namespace (takes precedence over selector).
	MachineName string `json:"machineName,omitempty"`
	// Selector matches Machine labels when MachineName is empty.
	Selector map[string]string `json:"selector,omitempty"`
	Policy   VmNetworkPolicy   `json:"policy"`
	// CNP is an optional FluxVM CiliumNetworkPolicy-shaped document posted to /v1/network/cnp.
	CNP map[string]any `json:"cnp,omitempty"`
	// Cilium controls optional sync to a real cluster CiliumNetworkPolicy CR.
	Cilium *MachineNetworkPolicyCilium `json:"cilium,omitempty"`
}

// MachineNetworkPolicyCilium is opt-in sync onto cilium.io/v2 CiliumNetworkPolicy.
type MachineNetworkPolicyCilium struct {
	Sync       bool   `json:"sync,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
}

type MachineNetworkPolicyStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty -- this whole status is patched as one object; under
	// JSON merge-patch semantics an absent key never clears a
	// previously-set value (see model.MachineStatus.Message's comment).
	Message          string     `json:"message"`
	ObservedMachines int        `json:"observedMachines,omitempty"`
	LastAppliedTime  *time.Time `json:"lastAppliedTime,omitempty"`
	EffectiveSynced  bool       `json:"effectiveSynced,omitempty"`
	// AppliedMachines is the ground truth of which Machine names (in this
	// policy's own namespace) this policy actually pushed Spec.Policy onto
	// on the most recent successful reconcile -- not just which Machines
	// currently match Spec.Selector/MachineName. The prior tick's value is
	// what reconcileMachineNetworkPolicy diffs against to notice a Machine
	// that fell out of selection (a selector edit, MachineName change, or a
	// Machine's own labels changing) while it kept running, the only case
	// nothing else ever resets: object deletion already resets every
	// currently-selected Machine before removing FinalizerNetworkPolicy,
	// and ensureStopped/ensureHalted tear down the VM's whole network
	// dataplane regardless of policy. See reconcileMachineNetworkPolicy's
	// own doc comment for why this mirrors
	// MachineStatus.AppliedServiceFabricMemberships's identical role.
	AppliedMachines []string `json:"appliedMachines,omitempty"`
	// CiliumNetworkPolicyRef is the namespaced name of the synced CNP, when any.
	CiliumNetworkPolicyRef string `json:"ciliumNetworkPolicyRef,omitempty"`
	CiliumSyncMessage      string `json:"ciliumSyncMessage,omitempty"`
}

func (p MachineNetworkPolicy) Namespace() string {
	if p.Metadata.Namespace == "" {
		return DefaultNamespace
	}
	return p.Metadata.Namespace
}

// NetworkSecurityGroup maps to FluxVM POST /v1/network/groups.
type NetworkSecurityGroup struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                 `json:"metadata"`
	Spec     NetworkSecurityGroupSpec   `json:"spec"`
	Status   NetworkSecurityGroupStatus `json:"status,omitempty"`
}

type NetworkSecurityGroupList struct {
	TypeMeta `json:",inline"`
	Items    []NetworkSecurityGroup `json:"items"`
}

type NetworkSecurityGroupSpec struct {
	// GroupName is the FluxVM group name; defaults to metadata.name.
	GroupName   string          `json:"groupName,omitempty"`
	Labels      []string        `json:"labels,omitempty"`
	Priority    uint32          `json:"priority,omitempty"`
	Description string          `json:"description,omitempty"`
	Policy      VmNetworkPolicy `json:"policy,omitempty"`
}

type NetworkSecurityGroupStatus struct {
	Phase string `json:"phase,omitempty"`
	// No omitempty -- see MachineStatus.Message's comment.
	Message   string `json:"message"`
	Identity  uint32 `json:"identity,omitempty"`
	AppliedOn string `json:"appliedOn,omitempty"` // node that last upserted
}

func (g NetworkSecurityGroup) Namespace() string {
	if g.Metadata.Namespace == "" {
		return DefaultNamespace
	}
	return g.Metadata.Namespace
}

func (g NetworkSecurityGroup) FluxGroupName() string {
	if g.Spec.GroupName != "" {
		return g.Spec.GroupName
	}
	return g.Metadata.Name
}

// LabelsMatch returns true when every selector key/value is present on labels.
func LabelsMatch(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels == nil || labels[k] != v {
			return false
		}
	}
	return true
}

// vmNetworkPolicyL4Protocols mirrors FluxVM's own
// crates/fluxvm-network/src/ebpf.rs parse_port_rule exactly: the set of
// proto tokens its eBPF dataplane accepts in an AllowPorts entry.
var vmNetworkPolicyL4Protocols = map[string]bool{
	"tcp": true, "udp": true, "sctp": true, "icmp": true, "icmp6": true, "icmpv6": true,
}

// ValidateVmNetworkPolicy checks the free-form AllowCidrs/DenyCidrs/
// AllowPorts/MaxEgress* fields of a VmNetworkPolicy against exactly the
// same syntax FluxVM's own ebpf::validate_policy enforces server-side
// (crates/fluxvm-network/src/ebpf.rs, called from groups.rs's
// UpsertNetworkGroup handler and dataplane.rs's SetVMNetworkPolicy
// handler) -- deliberately reimplemented here rather than shared:
// Kairon is a pure Go module with no dependency on FluxVM's Rust crates,
// and this is a small, stable contract.
//
// Until kairon-controller's admission webhook called this
// (validateMachineNetworkPolicy/validateNetworkSecurityGroup in
// internal/controller/webhook.go), a malformed entry -- a CIDR missing
// its /prefix, a port rule using an unsupported protocol, an
// out-of-range prefix, maxEgressMbps/maxEgressPps explicitly set to
// zero -- sailed straight through `kubectl apply` and only ever failed
// once kairon-node's agent actually tried to apply it, at which point
// reconcileSecurityGroup/reconcileMachineNetworkPolicy
// (internal/agent/network.go) set the object's status to Error and
// retried it, forever, on every subsequent reconcile tick, on every
// node the policy selected a Machine on -- a syntax error that can
// never self-heal, spamming logs and status updates indefinitely
// instead of failing once, clearly, at write time.
func ValidateVmNetworkPolicy(p VmNetworkPolicy) error {
	for _, cidr := range p.AllowCidrs {
		if err := validateNetworkPolicyCIDR(cidr); err != nil {
			return fmt.Errorf("allowCidrs: %w", err)
		}
	}
	for _, cidr := range p.DenyCidrs {
		if err := validateNetworkPolicyCIDR(cidr); err != nil {
			return fmt.Errorf("denyCidrs: %w", err)
		}
	}
	for _, rule := range p.AllowPorts {
		if err := validateNetworkPolicyPortRule(rule); err != nil {
			return fmt.Errorf("allowPorts: %w", err)
		}
	}
	if p.MaxEgressMbps != nil && *p.MaxEgressMbps == 0 {
		return fmt.Errorf("maxEgressMbps must be greater than zero when set")
	}
	if p.MaxEgressPps != nil && *p.MaxEgressPps == 0 {
		return fmt.Errorf("maxEgressPps must be greater than zero when set")
	}
	if p.MaxIngressMbps != nil && *p.MaxIngressMbps == 0 {
		return fmt.Errorf("maxIngressMbps must be greater than zero when set")
	}
	if p.MaxIngressPps != nil && *p.MaxIngressPps == 0 {
		return fmt.Errorf("maxIngressPps must be greater than zero when set")
	}
	if err := validateNameList("allowSNI", p.AllowSNI); err != nil {
		return err
	}
	if err := validateNameList("allowDNS", p.AllowDNS); err != nil {
		return err
	}
	return nil
}

// DefaultMAC is the MAC a Machine gets when its netns tap leaves
// spec.network.mac empty: QEMU's 52:54:00 prefix plus 24 bits of FNV-1a over
// namespace and name, so a recreate or migration keeps the same address.
func DefaultMAC(namespace, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespace))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(name))
	v := h.Sum32()
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", byte(v>>16), byte(v>>8), byte(v))
}

// EffectiveNetwork is spec.network as sent to FluxVM: a netns tap without a
// MAC gets DefaultMAC, since FluxVM cannot create a netns NIC without one.
func (m Machine) EffectiveNetwork() NetworkSpec {
	n := m.Spec.Network
	if n.VirtualNetwork != nil {
		n.Mode = "tap"
	}
	if n.Mode == "tap" && (n.NetNS || n.VirtualNetwork != nil) && n.MAC == "" {
		n.MAC = DefaultMAC(m.Namespace(), m.Metadata.Name)
	}
	return n
}

// ValidateNetworkQoS rejects an explicit zero bucket.
func ValidateNetworkQoS(q *NetworkQoS) error {
	if q == nil {
		return nil
	}
	for _, pair := range []struct {
		name string
		v    *uint32
	}{
		{"ingressMbps", q.IngressMbps},
		{"egressMbps", q.EgressMbps},
		{"ingressPps", q.IngressPps},
		{"egressPps", q.EgressPps},
	} {
		if pair.v != nil && *pair.v == 0 {
			return fmt.Errorf("spec.network.qos.%s must be greater than zero when set", pair.name)
		}
	}
	return nil
}

func validateNameList(field string, names []string) error {
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || strings.ContainsAny(name, " /:\\") {
			return fmt.Errorf("%s: invalid name %q", field, raw)
		}
		body := strings.TrimPrefix(name, "*.")
		if body == "" || strings.HasPrefix(body, ".") || strings.HasSuffix(body, ".") {
			return fmt.Errorf("%s: invalid name %q", field, raw)
		}
		for _, label := range strings.Split(body, ".") {
			if label == "" || strings.Contains(label, "*") {
				return fmt.Errorf("%s: invalid name %q", field, raw)
			}
		}
	}
	return nil
}

// validateNetworkPolicyCIDR mirrors FluxVM's parse_ip_cidr: an address,
// a literal "/", and a prefix length (<=32 for IPv4, <=128 for IPv6).
// netip.ParsePrefix enforces exactly that shape and range on both
// families in one call -- unlike net.ParseCIDR, it rejects a bare
// address with no "/prefix" instead of silently accepting one.
func validateNetworkPolicyCIDR(raw string) error {
	if _, err := netip.ParsePrefix(raw); err != nil {
		return fmt.Errorf("invalid CIDR %q (must include /prefix): %w", raw, err)
	}
	return nil
}

// validateNetworkPolicyPortRule mirrors FluxVM's parse_port_rule:
// "proto/port" where proto is tcp/udp/sctp/icmp/icmp6/icmpv6
// (case-insensitive) and port is 1-65535, except icmp/icmp6/icmpv6
// where port 0 is allowed (those protocols have no port number; FluxVM
// only ever checks the field is present and parses as a plain uint16).
func validateNetworkPolicyPortRule(raw string) error {
	proto, portStr, ok := strings.Cut(raw, "/")
	if !ok {
		return fmt.Errorf("port rule %q must be proto/PORT", raw)
	}
	proto = strings.ToLower(strings.TrimSpace(proto))
	if !vmNetworkPolicyL4Protocols[proto] {
		return fmt.Errorf("port rule %q: unsupported protocol %q; use tcp, udp, sctp, icmp, or icmp6", raw, proto)
	}
	port, err := strconv.ParseUint(strings.TrimSpace(portStr), 10, 16)
	if err != nil {
		return fmt.Errorf("port rule %q: invalid port: %w", raw, err)
	}
	if port == 0 && proto != "icmp" && proto != "icmp6" && proto != "icmpv6" {
		return fmt.Errorf("port rule %q: port must be 1-65535", raw)
	}
	return nil
}
