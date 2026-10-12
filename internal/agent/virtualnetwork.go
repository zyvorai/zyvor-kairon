// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/bgp"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	defaultVnetSyncInterval = 10 * time.Second
	// vnetResyncAfter re-applies an unchanged network so a FluxVM that lost
	// its state (a reinstalled host) converges without a Kairon change.
	vnetResyncAfter = 5 * time.Minute
)

// VirtualNetworkConfig tunes how kairon-node applies VirtualNetworks.
type VirtualNetworkConfig struct {
	Disabled bool
	// Uplink is the host interface that carries SNAT and floating-IP
	// traffic; empty keeps virtual networks closed (no egress, no
	// floating IPs) on this node.
	Uplink string
	// NodeIP is the underlay address other nodes tunnel to and the SNAT
	// address; empty takes the Node's InternalIP.
	NodeIP string
	// Interval between syncs; zero is 10s.
	Interval time.Duration
	// BGP announces local floating IPs (and Service Fabric VIPs) when set.
	BGP *bgp.Speaker
	// AdvertiseServiceVIPs adds FluxVM's Service Fabric advertisements to BGP.
	AdvertiseServiceVIPs bool
	// AdvertisePrefixes are announced as well, e.g. a VNet's public range.
	AdvertisePrefixes []netip.Prefix
}

type vnetState struct {
	mu       sync.Mutex
	pushed   map[uint32]string
	pushedAt map[uint32]time.Time
	kick     chan struct{}
}

func (s *vnetState) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *vnetState) wakeChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kick == nil {
		s.kick = make(chan struct{}, 1)
	}
	return s.kick
}

// attachVirtualNetwork resolves m's spec.network.virtualNetwork allocation
// into m.Status.Network.VirtualNetwork, which the FluxVM create request
// reads and the status write keeps. It fails until the controller has
// allocated an address.
func (a *Agent) attachVirtualNetwork(ctx context.Context, m *model.Machine) error {
	ref := m.Spec.Network.VirtualNetwork
	if ref == nil {
		return nil
	}
	vn, err := a.Kube.GetVirtualNetwork(ctx, m.Namespace(), ref.Name)
	if err != nil {
		return fmt.Errorf("spec.network.virtualNetwork %q: %w", ref.Name, err)
	}
	st, err := machineVirtualNetworkStatus(vn, m.Metadata.Name)
	if err != nil {
		return err
	}
	ns := model.MachineNetworkStatus{}
	if m.Status.Network != nil {
		ns = *m.Status.Network
	}
	ns.VirtualNetwork = st
	m.Status.Network = &ns
	a.vnet.wake()
	return nil
}

func machineVirtualNetworkStatus(vn model.VirtualNetwork, machine string) (*model.MachineVirtualNetworkStatus, error) {
	if vn.Status.Phase != "Ready" || vn.Status.VNI == 0 {
		return nil, fmt.Errorf("virtual network %s is not ready: %s", vn.Metadata.Name, vn.Status.Message)
	}
	alloc, ok := vn.Status.AllocationFor(machine)
	if !ok {
		return nil, fmt.Errorf("waiting for an address on virtual network %s", vn.Metadata.Name)
	}
	p, err := model.VirtualNetworkPrefix(vn.Spec.CIDR)
	if err != nil {
		return nil, err
	}
	return &model.MachineVirtualNetworkStatus{
		Name:         vn.Metadata.Name,
		VNI:          vn.Status.VNI,
		IP:           alloc.IP,
		PrefixLength: p.Bits(),
		Gateway:      vn.Status.Gateway,
		MAC:          alloc.MAC,
		FloatingIP:   alloc.FloatingIP,
	}, nil
}

// DesiredVnetConfigs is what this node's FluxVM should route: every Ready
// network with a Machine placed here, with all placed endpoints. Floating
// IPs and SNAT need an uplink; floating IPs are applied only where their
// Machine runs, since that is where they are announced.
func DesiredVnetConfigs(vnets []model.VirtualNetwork, node, nodeIP, uplink string) (map[uint32]fluxvm.VnetConfig, []netip.Prefix) {
	out := map[uint32]fluxvm.VnetConfig{}
	var floating []netip.Prefix
	for _, vn := range vnets {
		if vn.Status.Phase != "Ready" || vn.Status.VNI == 0 {
			continue
		}
		local := false
		cfg := fluxvm.VnetConfig{
			VNI:         vn.Status.VNI,
			CIDR:        vn.Spec.CIDR,
			Gateway:     vn.Status.Gateway,
			FloatingIPs: []fluxvm.VnetFloatingIP{},
			Endpoints:   []fluxvm.VnetEndpoint{},
		}
		for _, a := range vn.Status.Allocations {
			if a.Node == node {
				local = true
				if a.FloatingIP != "" && uplink != "" {
					cfg.FloatingIPs = append(cfg.FloatingIPs, fluxvm.VnetFloatingIP{Address: a.FloatingIP, IP: a.IP})
					if addr, err := netip.ParseAddr(a.FloatingIP); err == nil {
						floating = append(floating, netip.PrefixFrom(addr, 32))
					}
				}
			}
			ip := a.NodeIP
			if a.Node == node && nodeIP != "" {
				ip = nodeIP
			}
			if ip == "" {
				continue
			}
			cfg.Endpoints = append(cfg.Endpoints, fluxvm.VnetEndpoint{IP: a.IP, MAC: a.MAC, NodeIP: ip})
		}
		if !local {
			continue
		}
		if uplink != "" {
			cfg.Uplink = uplink
			if vn.Spec.EgressSNAT && nodeIP != "" {
				cfg.SNATAddress = nodeIP
			}
		}
		out[cfg.VNI] = cfg
	}
	return out, floating
}

func (a *Agent) vnetNodeIP(ctx context.Context) string {
	if a.VirtualNetwork.NodeIP != "" {
		return a.VirtualNetwork.NodeIP
	}
	n, err := a.Kube.GetNode(ctx, a.NodeName)
	if err != nil {
		return ""
	}
	return model.NodeInternalIP(n)
}

// runVirtualNetworkSync applies VirtualNetworks to the local FluxVM and feeds
// the BGP speaker until ctx ends.
func (a *Agent) runVirtualNetworkSync(ctx context.Context) {
	if a.VirtualNetwork.Disabled {
		return
	}
	interval := a.VirtualNetwork.Interval
	if interval <= 0 {
		interval = defaultVnetSyncInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	kick := a.vnet.wakeChan()
	for {
		if err := a.syncVirtualNetworks(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("virtual network sync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

func (a *Agent) syncVirtualNetworks(ctx context.Context) error {
	vnets, err := a.Kube.ListVirtualNetworks(ctx)
	if err != nil {
		return fmt.Errorf("list virtual networks: %w", err)
	}
	nodeIP := a.vnetNodeIP(ctx)
	desired, floating := DesiredVnetConfigs(vnets, a.NodeName, nodeIP, a.VirtualNetwork.Uplink)
	if caps, err := a.Flux.GetRuntimeCapabilities(ctx); err != nil || !caps.Host.Vnet {
		if len(desired) == 0 {
			a.updateBGP(ctx, nil)
			return nil
		}
		if a.Metrics != nil {
			a.Metrics.ObserveVirtualNetworkSyncError()
		}
		if err != nil {
			return fmt.Errorf("FluxVM capabilities: %w", err)
		}
		return fmt.Errorf("%d virtual network(s) have Machines on this node but FluxVM lacks host.vnet", len(desired))
	}
	a.vnet.mu.Lock()
	if a.vnet.pushed == nil {
		a.vnet.pushed, a.vnet.pushedAt = map[uint32]string{}, map[uint32]time.Time{}
	}
	a.vnet.mu.Unlock()

	var firstErr error
	note := func(err error) {
		if err == nil {
			return
		}
		if a.Metrics != nil {
			a.Metrics.ObserveVirtualNetworkSyncError()
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	local, remote := 0, 0
	vnis := make([]uint32, 0, len(desired))
	for vni := range desired {
		vnis = append(vnis, vni)
	}
	sort.Slice(vnis, func(i, j int) bool { return vnis[i] < vnis[j] })
	for _, vni := range vnis {
		cfg := desired[vni]
		body, _ := json.Marshal(cfg)
		a.vnet.mu.Lock()
		same := a.vnet.pushed[vni] == string(body) && time.Since(a.vnet.pushedAt[vni]) < vnetResyncAfter
		a.vnet.mu.Unlock()
		if same {
			continue
		}
		st, err := a.Flux.PutVnet(ctx, cfg)
		if err != nil {
			note(fmt.Errorf("apply virtual network %d: %w", vni, err))
			continue
		}
		a.vnet.mu.Lock()
		a.vnet.pushed[vni], a.vnet.pushedAt[vni] = string(body), time.Now()
		a.vnet.mu.Unlock()
		a.Log.Info("applied virtual network", "vni", vni, "localEndpoints", len(st.LocalEndpoints), "remoteEndpoints", st.RemoteEndpoints)
	}

	applied, err := a.Flux.ListVnets(ctx)
	if err != nil {
		note(fmt.Errorf("list FluxVM virtual networks: %w", err))
	}
	for _, st := range applied {
		if _, ok := desired[st.Config.VNI]; ok {
			local += len(st.LocalEndpoints)
			remote += st.RemoteEndpoints
			continue
		}
		// FluxVM refuses while a VM is still attached; the next sync retries.
		if err := a.Flux.DeleteVnet(ctx, st.Config.VNI); err != nil {
			a.Log.Debug("virtual network not removed yet", "vni", st.Config.VNI, "error", err)
			continue
		}
		a.vnet.mu.Lock()
		delete(a.vnet.pushed, st.Config.VNI)
		delete(a.vnet.pushedAt, st.Config.VNI)
		a.vnet.mu.Unlock()
		a.Log.Info("removed virtual network", "vni", st.Config.VNI)
	}
	if a.Metrics != nil {
		a.Metrics.ObserveVirtualNetworks(len(desired), local, remote)
	}
	a.updateBGP(ctx, floating)
	return firstErr
}

func (a *Agent) updateBGP(ctx context.Context, floating []netip.Prefix) {
	sp := a.VirtualNetwork.BGP
	if sp == nil {
		return
	}
	routes := append([]netip.Prefix{}, floating...)
	routes = append(routes, a.VirtualNetwork.AdvertisePrefixes...)
	if a.VirtualNetwork.AdvertiseServiceVIPs {
		vips, err := a.Flux.ServiceAdvertisements(ctx)
		if err != nil {
			a.Log.Debug("service advertisements unavailable", "error", err)
		}
		routes = append(routes, vips...)
	}
	sp.SetRoutes(routes)
	if a.Metrics != nil {
		for _, s := range sp.Status() {
			a.Metrics.ObserveBGPSession(s.Peer, s.State == "Established", s.Advertised)
		}
	}
}
