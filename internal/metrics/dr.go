// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/kairon/internal/model"
)

// drMetrics covers MachineProtectionPlans -- registered by NewRecorder
// only, since kairon-controller runs them.
type drMetrics struct {
	rpo           *prometheus.GaugeVec
	active        *prometheus.GaugeVec
	peerReachable *prometheus.GaugeVec
	rto           *prometheus.GaugeVec
	dataLoss      *prometheus.GaugeVec
	testSuccess   *prometheus.GaugeVec
	testTime      *prometheus.GaugeVec
	dnsRecords    *prometheus.GaugeVec
}

func newDRMetrics(reg *prometheus.Registry) *drMetrics {
	d := &drMetrics{
		rpo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_rpo_seconds",
			Help: "Age of the oldest of a protection plan's newest restore points: replicated backups on the active site, imported ones on the standby site.",
		}, []string{"namespace", "plan"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_active",
			Help: "1 when this site runs a protection plan's Machines, 0 on the standby site.",
		}, []string{"namespace", "plan"}),
		peerReachable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_peer_reachable",
			Help: "1 when the last call to the protection plan's peer cluster succeeded.",
		}, []string{"namespace", "plan"}),
		rto: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_last_rto_seconds",
			Help: "Start to every Machine Running for the plan's last finished failover or DR test, by kind.",
		}, []string{"namespace", "plan", "kind"}),
		dataLoss: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_last_data_loss_seconds",
			Help: "Start of the plan's last failover or DR test minus the oldest restore point it used, by kind.",
		}, []string{"namespace", "plan", "kind"}),
		testSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_last_test_success",
			Help: "1 when the plan's newest finished DR test brought every copy up, 0 when it failed.",
		}, []string{"namespace", "plan"}),
		testTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_last_test_timestamp_seconds",
			Help: "Unix time the plan's newest DR test finished.",
		}, []string{"namespace", "plan"}),
		dnsRecords: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_dr_dns_records",
			Help: "network.dns records this site publishes in the plan's DNSEndpoint (0 on the standby).",
		}, []string{"namespace", "plan"}),
	}
	reg.MustRegister(d.rpo, d.active, d.peerReachable, d.rto, d.dataLoss, d.testSuccess, d.testTime, d.dnsRecords)
	return d
}

// ObserveProtectionPlan records one plan's status.
func (r *Recorder) ObserveProtectionPlan(namespace, plan string, st model.MachineProtectionPlanStatus) {
	if r == nil || r.dr == nil {
		return
	}
	if st.RPOSeconds != nil {
		r.dr.rpo.WithLabelValues(namespace, plan).Set(*st.RPOSeconds)
	} else {
		r.dr.rpo.DeleteLabelValues(namespace, plan)
	}
	r.dr.active.WithLabelValues(namespace, plan).Set(boolGauge(st.Active))
	r.dr.peerReachable.WithLabelValues(namespace, plan).Set(boolGauge(st.Peer != nil && st.Peer.Reachable))
	r.dr.dnsRecords.WithLabelValues(namespace, plan).Set(float64(st.DNSRecords))
	if lt := st.LastTest; lt != nil {
		r.dr.testSuccess.WithLabelValues(namespace, plan).Set(boolGauge(lt.Succeeded))
		if lt.CompletedAt != nil {
			r.dr.testTime.WithLabelValues(namespace, plan).Set(float64(lt.CompletedAt.Unix()))
		}
	}
	for kind, run := range map[string]*model.ProtectionRun{"failover": st.Failover, "test": st.Test} {
		if run == nil {
			continue
		}
		if run.RTOSeconds != nil {
			r.dr.rto.WithLabelValues(namespace, plan, kind).Set(*run.RTOSeconds)
		}
		if run.DataLossSeconds != nil {
			r.dr.dataLoss.WithLabelValues(namespace, plan, kind).Set(*run.DataLossSeconds)
		}
	}
}
