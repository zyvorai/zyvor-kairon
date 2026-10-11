// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// topologyMetrics covers the topology scheduler -- registered by
// NewRecorder only, since kairon-controller makes every placement.
type topologyMetrics struct {
	failures     *prometheus.CounterVec
	freeNUMACPUs *prometheus.GaugeVec
}

func newTopologyMetrics(reg *prometheus.Registry) *topologyMetrics {
	m := &topologyMetrics{
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_topology_allocation_failures_total",
			Help: "Scheduling attempts of spec.topology/spec.devices Machines that found no node, by reason (TopologyUnsatisfiable, DRAAllocationPending, Unschedulable).",
		}, []string{"reason"}),
		freeNUMACPUs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_numa_free_cpus",
			Help: "Pinnable CPUs no Machine holds, by node and NUMA node (nodes with a NodeTopology).",
		}, []string{"node", "numa"}),
	}
	reg.MustRegister(m.failures, m.freeNUMACPUs)
	return m
}

// ObserveTopologyFailure counts one failed topology placement.
func (r *Recorder) ObserveTopologyFailure(reason string) {
	if r == nil || r.topology == nil {
		return
	}
	r.topology.failures.WithLabelValues(reason).Inc()
}

// ObserveNUMAFreeCPUs replaces every node's per-NUMA free CPU gauges.
func (r *Recorder) ObserveNUMAFreeCPUs(free map[string]map[int]int) {
	if r == nil || r.topology == nil {
		return
	}
	r.topology.freeNUMACPUs.Reset()
	for node, byNUMA := range free {
		for numa, n := range byNUMA {
			r.topology.freeNUMACPUs.WithLabelValues(node, strconv.Itoa(numa)).Set(float64(n))
		}
	}
}
