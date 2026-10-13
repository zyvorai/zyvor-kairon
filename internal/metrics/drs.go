// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/kairon/internal/scheduler"
)

// placementMetrics covers node maintenance mode and DRS -- registered by
// NewRecorder only, since kairon-controller drives both.
type placementMetrics struct {
	maintenance          *prometheus.GaugeVec
	maintenanceRemaining *prometheus.GaugeVec
	imbalance            prometheus.Gauge
	utilization          *prometheus.GaugeVec
	moves                *prometheus.CounterVec
}

func newPlacementMetrics(reg *prometheus.Registry) *placementMetrics {
	p := &placementMetrics{
		maintenance: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_maintenance",
			Help: "1 for a node in maintenance mode, by state: Entering (Machines still leaving) or InMaintenance.",
		}, []string{"node", "state"}),
		maintenanceRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_node_maintenance_remaining_machines",
			Help: "Machines still on a node in maintenance mode.",
		}, []string{"node"}),
		imbalance: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "kairon_drs_imbalance_percent",
			Help: "Spread, in percentage points, between the most and least utilized eligible node (reserved CPU or memory, whichever is higher; worst host PSI with -drs-signal=pressure), from the last DRS round.",
		}),
		utilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_drs_node_utilization_percent",
			Help: "Reserved share of a node's allocatable capacity as DRS saw it in its last round, by resource: cpu, memory, or max; with -drs-signal=pressure also pressure, the node's worst host PSI some avg60.",
		}, []string{"node", "resource"}),
		moves: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_drs_moves_total",
			Help: "DRS moves, by result: recommended (recommend mode), started (automatic mode created the MachineMigration), or blocked (a MachineDisruptionBudget refused it).",
		}, []string{"result"}),
	}
	reg.MustRegister(p.maintenance, p.maintenanceRemaining, p.imbalance, p.utilization, p.moves)
	return p
}

// ObserveNodeMaintenance records a node's maintenance state; an empty state
// clears the node's series once it leaves maintenance.
func (r *Recorder) ObserveNodeMaintenance(node, state string, remaining int) {
	if r == nil || r.placement == nil {
		return
	}
	r.placement.maintenance.DeletePartialMatch(prometheus.Labels{"node": node})
	if state == "" {
		r.placement.maintenanceRemaining.DeleteLabelValues(node)
		return
	}
	r.placement.maintenance.WithLabelValues(node, state).Set(1)
	r.placement.maintenanceRemaining.WithLabelValues(node).Set(float64(remaining))
}

// ObserveDRSPlan records one DRS round's view of the cluster.
func (r *Recorder) ObserveDRSPlan(plan scheduler.RebalancePlan) {
	if r == nil || r.placement == nil {
		return
	}
	r.placement.imbalance.Set(float64(plan.ImbalanceBefore))
	r.placement.utilization.Reset()
	for _, u := range plan.Nodes {
		r.placement.utilization.WithLabelValues(u.Node, "cpu").Set(float64(u.CPUPercent))
		r.placement.utilization.WithLabelValues(u.Node, "memory").Set(float64(u.MemoryPercent))
		r.placement.utilization.WithLabelValues(u.Node, "max").Set(float64(u.Percent))
		if u.Pressure != nil {
			r.placement.utilization.WithLabelValues(u.Node, "pressure").Set(u.Pressure.Max())
		}
	}
}

// ObserveDRSMove counts one DRS move by result.
func (r *Recorder) ObserveDRSMove(result string) {
	if r == nil || r.placement == nil {
		return
	}
	r.placement.moves.WithLabelValues(result).Inc()
}
