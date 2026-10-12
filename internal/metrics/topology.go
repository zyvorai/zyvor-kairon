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
	vram         *prometheus.GaugeVec
	gpus         *prometheus.GaugeVec
	vgpuFree     *prometheus.GaugeVec
}

// NodeGPUs is one node's GPU picture for ObserveGPUs.
type NodeGPUs struct {
	VRAMTotalMiB     int64
	VRAMAllocatedMiB int64
	// ByHealth counts GPUs and vGPU parents by Healthy, Degraded,
	// Unhealthy or Unknown.
	ByHealth map[string]int
	// VGPUAvailable is instances left by parent and profile (mdev type).
	VGPUAvailable map[[2]string]int
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
		vram: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_gpu_vram_mib",
			Help: "GPU memory per node from NodeTopology (state=total) and held by Machines' whole GPUs and vGPU framebuffers (state=allocated).",
		}, []string{"node", "state"}),
		gpus: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_gpu_devices",
			Help: "GPUs and vGPU parents per node by health (Healthy, Degraded, Unhealthy, Unknown).",
		}, []string{"node", "health"}),
		vgpuFree: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_vgpu_available_instances",
			Help: "vGPU instances a parent can still create, by node, parent and profile (mdev type).",
		}, []string{"node", "parent", "profile"}),
	}
	reg.MustRegister(m.failures, m.freeNUMACPUs, m.vram, m.gpus, m.vgpuFree)
	return m
}

// ObserveGPUs replaces every node's GPU gauges.
func (r *Recorder) ObserveGPUs(nodes map[string]NodeGPUs) {
	if r == nil || r.topology == nil {
		return
	}
	t := r.topology
	t.vram.Reset()
	t.gpus.Reset()
	t.vgpuFree.Reset()
	for node, g := range nodes {
		t.vram.WithLabelValues(node, "total").Set(float64(g.VRAMTotalMiB))
		t.vram.WithLabelValues(node, "allocated").Set(float64(g.VRAMAllocatedMiB))
		for h, n := range g.ByHealth {
			t.gpus.WithLabelValues(node, h).Set(float64(n))
		}
		for k, n := range g.VGPUAvailable {
			t.vgpuFree.WithLabelValues(node, k[0], k[1]).Set(float64(n))
		}
	}
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
