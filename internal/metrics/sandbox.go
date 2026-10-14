// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import "github.com/prometheus/client_golang/prometheus"

// sandboxMetrics covers spec.lifecycle and MachineClaim usage --
// registered by NewRecorder only, since kairon-controller drives both.
type sandboxMetrics struct {
	lifecycleActions *prometheus.CounterVec
	claimSeconds     *prometheus.CounterVec
	claimVCPUSeconds *prometheus.CounterVec
	claimMemGiBSecs  *prometheus.CounterVec
}

func newSandboxMetrics(reg *prometheus.Registry) *sandboxMetrics {
	s := &sandboxMetrics{
		lifecycleActions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_lifecycle_actions_total",
			Help: "spec.lifecycle actions kairon-controller applied, by action: Pause, Hibernate, Halt, Stop, Delete (idle), or Expire (maxLifetimeSeconds).",
		}, []string{"action"}),
		claimSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_claim_usage_seconds_total",
			Help: "Seconds bound MachineClaims spent with their Machine active (Running) or suspended (Paused, Hibernated, Halted), by namespace.",
		}, []string{"namespace", "state"}),
		claimVCPUSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_claim_usage_vcpu_seconds_total",
			Help: "vCPU-seconds of Running Machines bound to MachineClaims, by namespace.",
		}, []string{"namespace"}),
		claimMemGiBSecs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_claim_usage_memory_gib_seconds_total",
			Help: "Memory GiB-seconds of Running Machines bound to MachineClaims, by namespace.",
		}, []string{"namespace"}),
	}
	reg.MustRegister(s.lifecycleActions, s.claimSeconds, s.claimVCPUSeconds, s.claimMemGiBSecs)
	return s
}

// ObserveLifecycleAction counts one spec.lifecycle action.
func (r *Recorder) ObserveLifecycleAction(action string) {
	if r == nil || r.sandbox == nil {
		return
	}
	r.sandbox.lifecycleActions.WithLabelValues(action).Inc()
}

// ObserveClaimUsage adds one accrual step for a bound MachineClaim.
func (r *Recorder) ObserveClaimUsage(namespace string, activeSeconds, suspendedSeconds, vcpuSeconds, memGiBSeconds float64) {
	if r == nil || r.sandbox == nil {
		return
	}
	if activeSeconds > 0 {
		r.sandbox.claimSeconds.WithLabelValues(namespace, "active").Add(activeSeconds)
	}
	if suspendedSeconds > 0 {
		r.sandbox.claimSeconds.WithLabelValues(namespace, "suspended").Add(suspendedSeconds)
	}
	if vcpuSeconds > 0 {
		r.sandbox.claimVCPUSeconds.WithLabelValues(namespace).Add(vcpuSeconds)
	}
	if memGiBSeconds > 0 {
		r.sandbox.claimMemGiBSecs.WithLabelValues(namespace).Add(memGiBSeconds)
	}
}
