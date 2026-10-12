// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import "github.com/prometheus/client_golang/prometheus"

// vnetMetrics covers virtual networks and the BGP speaker -- registered by
// NewNodeRecorder only, since kairon-node pushes networks to FluxVM.
type vnetMetrics struct {
	networks   prometheus.Gauge
	endpoints  *prometheus.GaugeVec
	syncErrors prometheus.Counter
	bgpUp      *prometheus.GaugeVec
	bgpRoutes  *prometheus.GaugeVec
}

func newVnetMetrics(reg *prometheus.Registry) *vnetMetrics {
	m := &vnetMetrics{
		networks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kairon_node_virtual_networks",
			Help: "Virtual networks kairon-node has applied to the local FluxVM.",
		}),
		endpoints: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_virtual_network_endpoints",
			Help: "Endpoints of the virtual networks applied on this node, by location (local, remote).",
		}, []string{"location"}),
		syncErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kairon_node_virtual_network_sync_errors_total",
			Help: "Failed attempts to apply or remove a virtual network on the local FluxVM.",
		}),
		bgpUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_bgp_session_up",
			Help: "1 while the BGP session to the peer is Established.",
		}, []string{"peer"}),
		bgpRoutes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_bgp_advertised_prefixes",
			Help: "Prefixes kairon-node announces to the peer.",
		}, []string{"peer"}),
	}
	reg.MustRegister(m.networks, m.endpoints, m.syncErrors, m.bgpUp, m.bgpRoutes)
	return m
}

// ObserveVirtualNetworks records what the last sync applied.
func (r *Recorder) ObserveVirtualNetworks(networks, local, remote int) {
	if r == nil || r.vnet == nil {
		return
	}
	r.vnet.networks.Set(float64(networks))
	r.vnet.endpoints.WithLabelValues("local").Set(float64(local))
	r.vnet.endpoints.WithLabelValues("remote").Set(float64(remote))
}

// ObserveVirtualNetworkSyncError counts one failed apply or remove.
func (r *Recorder) ObserveVirtualNetworkSyncError() {
	if r == nil || r.vnet == nil {
		return
	}
	r.vnet.syncErrors.Inc()
}

// ObserveBGPSession records one peer's session state and announced prefixes.
func (r *Recorder) ObserveBGPSession(peer string, established bool, advertised int) {
	if r == nil || r.vnet == nil {
		return
	}
	r.vnet.bgpUp.WithLabelValues(peer).Set(boolGauge(established))
	r.vnet.bgpRoutes.WithLabelValues(peer).Set(float64(advertised))
}
