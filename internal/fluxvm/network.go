// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// WireVmNetworkPolicy is the snake_case JSON FluxVM expects for VmNetworkPolicy.
type WireVmNetworkPolicy struct {
	DefaultAllow   bool     `json:"default_allow"`
	AllowCidrs     []string `json:"allow_cidrs,omitempty"`
	DenyCidrs      []string `json:"deny_cidrs,omitempty"`
	AllowPorts     []string `json:"allow_ports,omitempty"`
	MaxEgressMbps  *uint32  `json:"max_egress_mbps,omitempty"`
	MaxEgressPps   *uint32  `json:"max_egress_pps,omitempty"`
	AllowFqdns     []string `json:"allow_fqdns,omitempty"`
	Groups         []string `json:"groups,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Entities       []string `json:"entities,omitempty"`
	AuditMode      bool     `json:"audit_mode,omitempty"`
	AllowIcmp      bool     `json:"allow_icmp,omitempty"`
	SampleRate     uint32   `json:"sample_rate,omitempty"`
	AllowSNI       []string `json:"allow_sni,omitempty"`
	AllowDNS       []string `json:"allow_dns,omitempty"`
	MaxIngressMbps *uint32  `json:"max_ingress_mbps,omitempty"`
	MaxIngressPps  *uint32  `json:"max_ingress_pps,omitempty"`
}

func ToWirePolicy(p model.VmNetworkPolicy) WireVmNetworkPolicy {
	return WireVmNetworkPolicy{
		DefaultAllow:   p.DefaultAllow,
		AllowCidrs:     p.AllowCidrs,
		DenyCidrs:      p.DenyCidrs,
		AllowPorts:     p.AllowPorts,
		MaxEgressMbps:  p.MaxEgressMbps,
		MaxEgressPps:   p.MaxEgressPps,
		AllowFqdns:     p.AllowFqdns,
		Groups:         p.Groups,
		Labels:         p.Labels,
		Entities:       p.Entities,
		AuditMode:      p.AuditMode,
		AllowIcmp:      p.AllowIcmp,
		SampleRate:     p.SampleRate,
		AllowSNI:       p.AllowSNI,
		AllowDNS:       p.AllowDNS,
		MaxIngressMbps: p.MaxIngressMbps,
		MaxIngressPps:  p.MaxIngressPps,
	}
}

// FromWirePolicy is ToWirePolicy's inverse -- decodes what FluxVM's own
// GET /v1/vms/{id}/network/policy reports back into Kairon's model type,
// for drift-checking against what SetVMNetworkPolicy last sent (FluxVM's
// own policy engine is free to normalize/default fields on write, so a
// read-back is the only way to see what's actually enforced, not just
// what was last requested).
func FromWirePolicy(w WireVmNetworkPolicy) model.VmNetworkPolicy {
	return model.VmNetworkPolicy{
		DefaultAllow:   w.DefaultAllow,
		AllowCidrs:     w.AllowCidrs,
		DenyCidrs:      w.DenyCidrs,
		AllowPorts:     w.AllowPorts,
		MaxEgressMbps:  w.MaxEgressMbps,
		MaxEgressPps:   w.MaxEgressPps,
		AllowFqdns:     w.AllowFqdns,
		Groups:         w.Groups,
		Labels:         w.Labels,
		Entities:       w.Entities,
		AuditMode:      w.AuditMode,
		AllowIcmp:      w.AllowIcmp,
		SampleRate:     w.SampleRate,
		AllowSNI:       w.AllowSNI,
		AllowDNS:       w.AllowDNS,
		MaxIngressMbps: w.MaxIngressMbps,
		MaxIngressPps:  w.MaxIngressPps,
	}
}

// GetVMNetworkPolicy reads back the policy FluxVM is actually enforcing
// for id (GET /v1/vms/{id}/network/policy) -- the read half of
// SetVMNetworkPolicy, previously write-only from Kairon's side.
func (c *Client) GetVMNetworkPolicy(ctx context.Context, id string) (*model.VmNetworkPolicy, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/policy", nil)
	if err != nil {
		return nil, err
	}
	var w WireVmNetworkPolicy
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("decode network policy: %w", err)
	}
	p := FromWirePolicy(w)
	return &p, nil
}

// DataplaneStatus is FluxVM GET /v1/vms/{id}/network/status.
type DataplaneStatus struct {
	Mode              string              `json:"mode"`
	Required          bool                `json:"required"`
	Attached          bool                `json:"attached"`
	Interface         string              `json:"interface,omitempty"`
	Identity          uint32              `json:"identity"`
	PinDir            string              `json:"pin_dir,omitempty"`
	SchemaVersion     *uint32             `json:"schema_version,omitempty"`
	SchemaCompatible  bool                `json:"schema_compatible"`
	PolicySynced      bool                `json:"policy_synced"`
	PolicyFingerprint *uint64             `json:"policy_fingerprint,omitempty"`
	Policy            WireVmNetworkPolicy `json:"policy,omitempty"`
}

// SecurityGroup is FluxVM /v1/network/groups body.
type SecurityGroup struct {
	Name        string              `json:"name"`
	Labels      []string            `json:"labels,omitempty"`
	Policy      WireVmNetworkPolicy `json:"policy"`
	Identity    uint32              `json:"identity,omitempty"`
	Priority    uint32              `json:"priority,omitempty"`
	Description string              `json:"description,omitempty"`
}

// ServiceBackend is one VIP backend entry.
type ServiceBackend struct {
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Weight  uint16 `json:"weight,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	State   string `json:"state,omitempty"`
}

// ServiceSpec is FluxVM /v1/network/services body (partial; membership updates merge backends).
type ServiceSpec struct {
	Name     string           `json:"name"`
	VIP      string           `json:"vip,omitempty"`
	Port     uint16           `json:"port,omitempty"`
	Protocol string           `json:"protocol,omitempty"`
	Backends []ServiceBackend `json:"backends,omitempty"`
}

// QgaIPAddress and QgaNetworkInterface mirror FluxVM's GET
// /v1/vms/{id}/qga/network-interfaces response -- the real
// qemu-guest-agent's own reported interfaces/IPs, straight from the
// guest's kernel, not a host-side DHCP lease guess.
type QgaIPAddress struct {
	IPAddress     string `json:"ip-address"`
	IPAddressType string `json:"ip-address-type"`
	Prefix        uint8  `json:"prefix"`
}

type QgaNetworkInterface struct {
	Name            string         `json:"name"`
	HardwareAddress string         `json:"hardware-address,omitempty"`
	IPAddresses     []QgaIPAddress `json:"ip-addresses,omitempty"`
}

// QGANetworkInterfaces calls FluxVM's real qemu-guest-agent
// guest-network-get-interfaces. Only meaningful for a VM created with
// spec.guestAgent.enabled -- FluxVM returns a clear error otherwise
// ("QGA not enabled for this VM").
func (c *Client) QGANetworkInterfaces(ctx context.Context, id string) ([]QgaNetworkInterface, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/qga/network-interfaces", nil)
	if err != nil {
		return nil, err
	}
	var out []QgaNetworkInterface
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode qga network-interfaces: %w", err)
	}
	return out, nil
}

// AllGuestIPs returns every IPv4/IPv6 address the guest agent reports
// across every non-loopback interface, IPv4 addresses first (in interface
// order), then IPv6 (in interface order) -- not the literal wire order,
// so that AllGuestIPs(ifaces)[0] always agrees with BestGuestIP(ifaces).
// Backs status.guestIPs; status.guestIP (BestGuestIP, below) stays a
// single string for backward compatibility.
func AllGuestIPs(ifaces []QgaNetworkInterface) []string {
	var v4, v6 []string
	for _, iface := range ifaces {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			switch addr.IPAddressType {
			case "ipv4":
				v4 = append(v4, addr.IPAddress)
			case "ipv6":
				v6 = append(v6, addr.IPAddress)
			}
		}
	}
	return append(v4, v6...)
}

// BestGuestIP picks the single "primary" address for status.guestIP: the
// first IPv4 address found across every non-loopback interface, falling
// back to the first IPv6 address only when no IPv4 address exists anywhere
// -- previously this returned "" in the IPv6-only case, silently reporting
// no guest IP at all for an IPv6-only guest. See AllGuestIPs for the full
// address list this is derived from. See BestGuestIPWithPrimary below for
// the SR-IOV/VFIO-NIC-aware variant reconcile actually calls.
func BestGuestIP(ifaces []QgaNetworkInterface) string {
	return BestGuestIPWithPrimary(ifaces, "")
}

// BestGuestIPWithPrimary is BestGuestIP, but prefers the interface whose
// hardware-address matches primaryMAC (case-insensitive) over blind
// first-IPv4 selection, when a match exists and reports an address at
// all. primaryMAC is normally FluxVM's own record of the primary
// virtio-net NIC's assigned MAC (Record.Request.Network.MAC) -- a real,
// necessary distinction once a Machine can also carry an SR-IOV VFIO NIC
// passed through via spec.deviceClaims (see docs/guides/machine-sriov.md):
// a passthrough VF keeps its own real hardware MAC (never one FluxVM
// assigns), and the guest's own interface enumeration order is outside
// Kairon's control, so without this a VF with its own DHCP lease could
// nondeterministically clobber status.guestIP instead of the intended
// primary management NIC. primaryMAC == "" (unknown, or a Machine with no
// virtio-net primary at all) falls back to the original first-IPv4/
// first-IPv6 behavior unchanged.
func BestGuestIPWithPrimary(ifaces []QgaNetworkInterface, primaryMAC string) string {
	if primaryMAC != "" {
		for _, iface := range ifaces {
			if iface.Name == "lo" || !strings.EqualFold(iface.HardwareAddress, primaryMAC) {
				continue
			}
			if addrs := AllGuestIPs([]QgaNetworkInterface{iface}); len(addrs) > 0 {
				return addrs[0]
			}
		}
	}
	all := AllGuestIPs(ifaces)
	if len(all) == 0 {
		return ""
	}
	return all[0]
}

func (c *Client) NetworkStatus(ctx context.Context, id string) (*DataplaneStatus, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/status", nil)
	if err != nil {
		return nil, err
	}
	var st DataplaneStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode network status: %w", err)
	}
	return &st, nil
}

// SetVMNetworkPolicy tolerates a 404 as success (nothing to set on a VM
// that's already gone) rather than folding it into a generic error like
// c.do does -- required so internal/agent/network.go's own
// MachineNetworkPolicy-deletion reset can fail closed on a genuine error
// while still treating "the VM is already gone" as nothing left to do, the
// same distinction Delete/DeleteNetworkGroup already draw for their own
// callers.
func (c *Client) SetVMNetworkPolicy(ctx context.Context, id string, policy model.VmNetworkPolicy) error {
	body, err := json.Marshal(ToWirePolicy(policy))
	if err != nil {
		return err
	}
	path := "/v1/vms/" + url.PathEscape(id) + "/network/policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fluxvm POST %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *Client) GetVMNetworkEffective(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/effective", nil)
}

// GetVMNetworkStats returns a VM's real, eBPF-dataplane-derived network
// byte/packet counters -- FluxVM's own GET /v1/vms/{id}/network/stats.
// Kept as raw JSON (like GetVMNetworkEffective above) rather than a typed
// struct: FluxVM's own handler returns a dynamic serde_json::Value here,
// not a fixed struct, so there is no stable Rust type to mirror exactly.
func (c *Client) GetVMNetworkStats(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/stats", nil)
}

// GetVMNetworkFlows returns a VM's most recent eBPF-observed network
// flows (up to limit, FluxVM's own default when limit is 0) -- FluxVM's
// own GET /v1/vms/{id}/network/flows. Raw JSON, same reasoning as
// GetVMNetworkStats.
func (c *Client) GetVMNetworkFlows(ctx context.Context, id string, limit int) (json.RawMessage, error) {
	path := "/v1/vms/" + url.PathEscape(id) + "/network/flows"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// GetVMNetworkDropReasons returns why the eBPF dataplane most recently
// dropped packets for a VM (up to limit) -- FluxVM's own
// GET /v1/vms/{id}/network/drop-reasons. The single most direct
// troubleshooting tool for "why is my MachineNetworkPolicy/
// NetworkSecurityGroup blocking traffic I expected to be allowed" --
// everything else (GetVMNetworkEffective, GetVMNetworkPolicy) shows what
// policy is configured; this shows what the dataplane is actually doing
// with real traffic. Raw JSON, same reasoning as GetVMNetworkStats.
func (c *Client) GetVMNetworkDropReasons(ctx context.Context, id string, limit int) (json.RawMessage, error) {
	path := "/v1/vms/" + url.PathEscape(id) + "/network/drop-reasons"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *Client) UpsertNetworkGroup(ctx context.Context, group SecurityGroup) (*SecurityGroup, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/network/groups", group)
	if err != nil {
		return nil, err
	}
	var out SecurityGroup
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode security group: %w", err)
	}
	return &out, nil
}

// DeleteNetworkGroup deletes a FluxVM network security group. Idempotent
// like Client.Delete (Machine deletion): a 404 -- the group is already
// gone, e.g. a retry after a prior attempt's finalizer-removal Patch
// failed but the FluxVM-side delete itself had already succeeded -- is
// treated as success rather than an error, so a caller that fails closed
// on a genuine delete error (see internal/agent/network.go's
// reconcileSecurityGroup) can retry safely without deadlocking on an
// already-completed deletion. Hand-rolled rather than routed through the
// shared do() helper specifically to get at the raw status code; do()
// treats every non-2xx (404 included) as an error.
func (c *Client) DeleteNetworkGroup(ctx context.Context, name string) error {
	return c.deleteIdempotent(ctx, "/v1/network/groups/"+url.PathEscape(name), "/v1/network/groups", name)
}

func (c *Client) ApplyCNP(ctx context.Context, doc map[string]any) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/network/cnp", doc)
	return err
}

func (c *Client) GetNetworkService(ctx context.Context, name string) (*ServiceSpec, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/network/services/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var out ServiceSpec
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode network service: %w", err)
	}
	return &out, nil
}

func (c *Client) UpsertNetworkService(ctx context.Context, spec ServiceSpec) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/network/services", spec)
	return err
}

// GetNetworkServiceIfExists is GetNetworkService's deregistration-path
// counterpart: a service that's already gone (404 -- deleted
// independently of any Machine, or simply never created) is nothing
// left to deregister a backend from, so this returns (nil, nil) rather
// than an error. GetNetworkService itself keeps treating any error, 404
// included, as fatal -- reconcileServiceFabric's registration path
// (internal/agent/network.go) wants joining a VIP that doesn't exist
// surfaced as the real misconfiguration it is, not silently ignored.
// Hand-rolled rather than routed through do(), same reasoning as
// DeleteNetworkGroup above: do() treats every non-2xx, 404 included, as
// an error.
func (c *Client) GetNetworkServiceIfExists(ctx context.Context, name string) (*ServiceSpec, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/network/services/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fluxvm GET /v1/network/services/%s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out ServiceSpec
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode network service: %w", err)
	}
	return &out, nil
}

func (c *Client) NetworkMigrationQuiesce(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/migration/quiesce", map[string]any{})
	return err
}

func (c *Client) NetworkMigrationExport(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/migration/export", nil)
}

func (c *Client) NetworkMigrationRestore(ctx context.Context, id string, snapshot json.RawMessage) error {
	var body any
	if len(snapshot) == 0 {
		body = map[string]any{}
	} else if err := json.Unmarshal(snapshot, &body); err != nil {
		body = snapshot
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/migration/restore", body)
	return err
}

func (c *Client) NetworkMigrationResume(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/migration/resume", map[string]any{})
	return err
}

func PolicyFingerprintString(fp *uint64) string {
	if fp == nil {
		return ""
	}
	return strconv.FormatUint(*fp, 10)
}

// BuildNetworkMap builds the FluxVM create `network` object (tagged enum JSON).
func BuildNetworkMap(n model.NetworkSpec) map[string]any {
	mode := n.Mode
	if mode == "" {
		mode = "user"
	}
	network := map[string]any{"mode": mode}
	switch mode {
	case "user":
		if len(n.Forwards) > 0 {
			forwards := make([]map[string]any, 0, len(n.Forwards))
			for _, f := range n.Forwards {
				proto := f.Protocol
				if proto == "" {
					proto = "tcp"
				}
				forwards = append(forwards, map[string]any{
					"host_port":  f.HostPort,
					"guest_port": f.GuestPort,
					"protocol":   proto,
				})
			}
			network["forwards"] = forwards
		}
	case "tap":
		if n.TapName != "" {
			network["tap_name"] = n.TapName
		}
		if n.Bridge != "" {
			network["bridge"] = n.Bridge
		}
		if n.MAC != "" {
			network["mac"] = n.MAC
		}
		if n.NetNS {
			network["netns"] = true
		}
	case "macvtap":
		if n.Parent != "" {
			network["parent"] = n.Parent
		}
		if n.MacvtapMode != "" {
			network["macvtap_mode"] = n.MacvtapMode
		}
		if n.MAC != "" {
			network["mac"] = n.MAC
		}
	default:
		if n.NetNS {
			network["netns"] = true
		}
		if n.Bridge != "" {
			network["bridge"] = n.Bridge
		}
		if n.Parent != "" {
			network["parent"] = n.Parent
		}
		if n.MAC != "" {
			network["mac"] = n.MAC
		}
		if n.TapName != "" {
			network["tap_name"] = n.TapName
		}
	}
	if n.DataplaneMode != "" {
		network["dataplane_mode"] = strings.ToLower(n.DataplaneMode)
	}
	if n.AntiSpoof {
		network["anti_spoof"] = true
	}
	if n.LearnIP {
		network["learn_ip"] = true
	}
	if n.QoS != nil {
		qos := map[string]any{}
		if n.QoS.IngressMbps != nil {
			qos["ingress_mbps"] = *n.QoS.IngressMbps
		}
		if n.QoS.EgressMbps != nil {
			qos["egress_mbps"] = *n.QoS.EgressMbps
		}
		if n.QoS.IngressPps != nil {
			qos["ingress_pps"] = *n.QoS.IngressPps
		}
		if n.QoS.EgressPps != nil {
			qos["egress_pps"] = *n.QoS.EgressPps
		}
		if len(qos) > 0 {
			network["qos"] = qos
		}
	}
	return network
}
