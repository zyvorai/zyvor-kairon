// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import "github.com/prometheus/client_golang/prometheus"

// memoryMetrics covers elastic memory -- registered by NewNodeRecorder only,
// since kairon-node drives every balloon.
type memoryMetrics struct {
	pressure *prometheus.GaugeVec
	balloon  *prometheus.GaugeVec
	actions  *prometheus.CounterVec
	psi      *prometheus.GaugeVec
}

func newMemoryMetrics(reg *prometheus.Registry) *memoryMetrics {
	m := &memoryMetrics{
		pressure: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_memory_pressure",
			Help: "1 while host memory is under pressure (MemAvailable below the low watermark or memory PSI some avg10 above the limit), by state: pressure, or exhausted when every elastic Machine is at its minimum.",
		}, []string{"node", "state"}),
		balloon: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_machine_balloon_mib",
			Help: "Memory an elastic-memory Machine's balloon holds, in MiB; kind is target (requested) or actual (reached by the guest).",
		}, []string{"namespace", "machine", "kind"}),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_memory_reclaim_actions_total",
			Help: "Balloon target changes kairon-node made, by direction (inflate, deflate).",
		}, []string{"direction"}),
		psi: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_psi_some_avg60_percent",
			Help: "Host PSI some avg60 kairon-node last published on its Node, by resource: cpu, memory, io.",
		}, []string{"node", "resource"}),
	}
	reg.MustRegister(m.pressure, m.balloon, m.actions, m.psi)
	return m
}

// ObserveNodeMemoryPressure sets the node's pressure and exhausted gauges.
func (r *Recorder) ObserveNodeMemoryPressure(node string, pressure, exhausted bool) {
	if r == nil || r.memory == nil {
		return
	}
	r.memory.pressure.WithLabelValues(node, "pressure").Set(boolGauge(pressure))
	r.memory.pressure.WithLabelValues(node, "exhausted").Set(boolGauge(exhausted))
}

// ObserveNodePSI sets the node's host PSI gauges.
func (r *Recorder) ObserveNodePSI(node string, cpu, memory, io float64) {
	if r == nil || r.memory == nil {
		return
	}
	r.memory.psi.WithLabelValues(node, "cpu").Set(cpu)
	r.memory.psi.WithLabelValues(node, "memory").Set(memory)
	r.memory.psi.WithLabelValues(node, "io").Set(io)
}

// ObserveMachineBalloon sets one Machine's balloon target and actual size.
func (r *Recorder) ObserveMachineBalloon(namespace, name string, targetMiB, actualMiB uint64) {
	if r == nil || r.memory == nil {
		return
	}
	r.memory.balloon.WithLabelValues(namespace, name, "target").Set(float64(targetMiB))
	r.memory.balloon.WithLabelValues(namespace, name, "actual").Set(float64(actualMiB))
}

// ObserveReclaimAction counts one balloon target change.
func (r *Recorder) ObserveReclaimAction(direction string) {
	if r == nil || r.memory == nil {
		return
	}
	r.memory.actions.WithLabelValues(direction).Inc()
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
