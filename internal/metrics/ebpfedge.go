// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// EdgeRecorder is the VM-edge eBPF metric set. It is separate from
// Recorder so existing controller/node registries stay unchanged.
type EdgeRecorder struct {
	Drops             *prometheus.CounterVec
	ConntrackRestored prometheus.Counter
	BlackholeMs       prometheus.Histogram
}

// NewEdgeRecorder registers attributed-drop and migration-continuity metrics.
func NewEdgeRecorder(reg prometheus.Registerer) *EdgeRecorder {
	r := &EdgeRecorder{
		Drops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_net_drops_total",
			Help: "VM-edge drops attributed to a MachineNetworkPolicy or dataplane guard.",
		}, []string{"namespace", "machine", "reason", "policy"}),
		ConntrackRestored: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kairon_net_conntrack_restored_total",
			Help: "Conntrack entries restored onto a destination during live migration.",
		}),
		BlackholeMs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "kairon_net_migration_blackhole_ms",
			Help:    "Milliseconds between conntrack export and restore.",
			Buckets: []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500},
		}),
	}
	reg.MustRegister(r.Drops, r.ConntrackRestored, r.BlackholeMs)
	return r
}

// ObserveDrop increments the attributed drop counter. An empty policy is
// recorded as "-" so the label set stays stable.
func (r *EdgeRecorder) ObserveDrop(namespace, machine, reason, policy string) {
	if r == nil {
		return
	}
	if policy == "" {
		policy = "-"
	}
	r.Drops.WithLabelValues(namespace, machine, reason, policy).Inc()
}

// ObserveDrops adds n packets to the attributed drop counter.
func (r *EdgeRecorder) ObserveDrops(namespace, machine, reason, policy string, n uint64) {
	if r == nil || n == 0 {
		return
	}
	if policy == "" {
		policy = "-"
	}
	r.Drops.WithLabelValues(namespace, machine, reason, policy).Add(float64(n))
}

// ObserveConntrackRestore records how many entries moved and how long the
// guest was black-holed.
func (r *EdgeRecorder) ObserveConntrackRestore(restored int, blackholeMs int64) {
	if r == nil || restored < 0 {
		return
	}
	r.ConntrackRestored.Add(float64(restored))
	r.BlackholeMs.Observe(float64(blackholeMs))
}
