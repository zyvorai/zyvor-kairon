// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

// VnetAttach is FluxVM's NetworkSpec::Tap.vnet: the guest's place on a
// virtual network.
type VnetAttach struct {
	VNI       uint32 `json:"vni"`
	IP        string `json:"ip"`
	PrefixLen int    `json:"prefix_len"`
	Gateway   string `json:"gateway"`
}

// VnetEndpoint is one guest of a virtual network and the host it runs on.
type VnetEndpoint struct {
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	NodeIP string `json:"node_ip"`
}

// VnetFloatingIP translates Address 1:1 to the guest at IP.
type VnetFloatingIP struct {
	Address string `json:"address"`
	IP      string `json:"ip"`
}

// VnetConfig is FluxVM's PUT /v1/network/vnets/{vni} body.
type VnetConfig struct {
	VNI         uint32           `json:"vni"`
	CIDR        string           `json:"cidr"`
	Gateway     string           `json:"gateway"`
	Uplink      string           `json:"uplink,omitempty"`
	SNATAddress string           `json:"snat_address,omitempty"`
	FloatingIPs []VnetFloatingIP `json:"floating_ips"`
	Endpoints   []VnetEndpoint   `json:"endpoints"`
}

// VnetStatus is one entry of FluxVM's GET /v1/network/vnets.
type VnetStatus struct {
	Config          VnetConfig `json:"config"`
	LocalEndpoints  []string   `json:"local_endpoints"`
	RemoteEndpoints int        `json:"remote_endpoints"`
	GatewayMAC      string     `json:"gateway_mac"`
}

// PutVnet applies a virtual network on this host.
func (c *Client) PutVnet(ctx context.Context, cfg VnetConfig) (*VnetStatus, error) {
	data, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/v1/network/vnets/%d", cfg.VNI), cfg)
	if err != nil {
		return nil, err
	}
	var out VnetStatus
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM vnet status: %w", err)
	}
	return &out, nil
}

// ListVnets reports the virtual networks applied on this host.
func (c *Client) ListVnets(ctx context.Context) ([]VnetStatus, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/network/vnets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []VnetStatus `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM vnet list: %w", err)
	}
	return out.Items, nil
}

// DeleteVnet removes a virtual network from this host; FluxVM refuses while
// a VM is still attached.
func (c *Client) DeleteVnet(ctx context.Context, vni uint32) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/v1/network/vnets/%d", vni), nil)
	return err
}

// ServiceAdvertisements is the Service Fabric VIPs FluxVM marks for
// announcement (GET /v1/network/services/advertisements, advertise=true).
func (c *Client) ServiceAdvertisements(ctx context.Context) ([]netip.Prefix, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/network/services/advertisements", nil)
	if err != nil {
		return nil, err
	}
	var snap struct {
		Items []struct {
			VIP       string `json:"vip"`
			PrefixLen int    `json:"prefix_len"`
			Advertise bool   `json:"advertise"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("decode FluxVM service advertisements: %w", err)
	}
	var out []netip.Prefix
	for _, it := range snap.Items {
		a, err := netip.ParseAddr(it.VIP)
		if !it.Advertise || err != nil || !a.Is4() {
			continue
		}
		bits := it.PrefixLen
		if bits <= 0 || bits > 32 {
			bits = 32
		}
		out = append(out, netip.PrefixFrom(a, bits).Masked())
	}
	return out, nil
}
