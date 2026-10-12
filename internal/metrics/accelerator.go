// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/kairon/internal/model"
)

// acceleratorMetrics covers MachineAcceleratorProfiles -- registered by
// NewRecorder only, since kairon-controller computes their status.
type acceleratorMetrics struct {
	capacity *prometheus.GaugeVec
	machines *prometheus.GaugeVec
}

func newAcceleratorMetrics(reg *prometheus.Registry) *acceleratorMetrics {
	a := &acceleratorMetrics{
		capacity: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_accelerator_profile_capacity",
			Help: "How many more Machines with an accelerator profile the cluster's free devices could take, by node (devices only, not CPUs or memory).",
		}, []string{"profile", "node"}),
		machines: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_accelerator_profile_machines",
			Help: "Machines whose spec.accelerators were expanded from an accelerator profile.",
		}, []string{"profile"}),
	}
	reg.MustRegister(a.capacity, a.machines)
	return a
}

// ObserveAcceleratorProfiles replaces every profile's series, so deleted
// profiles and nodes drop out.
func (r *Recorder) ObserveAcceleratorProfiles(profiles []model.MachineAcceleratorProfile) {
	if r == nil || r.accel == nil {
		return
	}
	r.accel.capacity.Reset()
	r.accel.machines.Reset()
	for _, p := range profiles {
		for _, n := range p.Status.Nodes {
			r.accel.capacity.WithLabelValues(p.Metadata.Name, n.Node).Set(float64(n.Fits))
		}
		r.accel.machines.WithLabelValues(p.Metadata.Name).Set(float64(p.Status.Machines))
	}
}
