// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zyvorai/kairon/internal/model"
)

// backupMetrics covers S3 (CBT) MachineBackups -- registered by
// NewRecorder only, since kairon-controller sees every backup finish.
type backupMetrics struct {
	bytes       *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	chainLength *prometheus.GaugeVec
	lastSuccess *prometheus.GaugeVec
}

func newBackupMetrics(reg *prometheus.Registry) *backupMetrics {
	m := &backupMetrics{
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kairon_backup_bytes_total",
			Help: "Bytes S3 MachineBackups stored (after compression), by kind (full/incremental/synthetic-full).",
		}, []string{"kind"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kairon_backup_duration_seconds",
			Help:    "Duration of finished S3 MachineBackups, by kind and result (succeeded/failed).",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400},
		}, []string{"kind", "result"}),
		chainLength: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_backup_chain_length",
			Help: "Successful backups in a policy Machine's current chain, by namespace, policy and machine.",
		}, []string{"namespace", "policy", "machine"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kairon_backup_last_success_timestamp",
			Help: "Point in time of a policy Machine's newest successful backup, by namespace, policy and machine.",
		}, []string{"namespace", "policy", "machine"}),
	}
	reg.MustRegister(m.bytes, m.duration, m.chainLength, m.lastSuccess)
	return m
}

// ObserveCBTBackup records an S3 MachineBackup reaching phase (Succeeded or
// Failed); kairon-controller reports each backup's transition once.
func (r *Recorder) ObserveCBTBackup(b model.MachineBackup, phase string) {
	if r == nil || r.backup == nil {
		return
	}
	kind := b.BackupMode()
	result := "failed"
	if phase == model.BackupSucceeded {
		result = "succeeded"
		if b.Status.Disk != nil {
			r.backup.bytes.WithLabelValues(kind).Add(float64(b.Status.Disk.StoredBytes))
		}
	}
	if !b.Metadata.CreationTimestamp.IsZero() {
		r.backup.duration.WithLabelValues(kind, result).Observe(time.Since(b.Metadata.CreationTimestamp).Seconds())
	}
}

// ObserveBackupChain sets one policy Machine's chain gauges; chainLength 0
// and a zero lastSuccess drop the series.
func (r *Recorder) ObserveBackupChain(namespace, policy, machine string, chainLength int, lastSuccess time.Time) {
	if r == nil || r.backup == nil {
		return
	}
	if chainLength == 0 {
		r.backup.chainLength.DeleteLabelValues(namespace, policy, machine)
	} else {
		r.backup.chainLength.WithLabelValues(namespace, policy, machine).Set(float64(chainLength))
	}
	if lastSuccess.IsZero() {
		r.backup.lastSuccess.DeleteLabelValues(namespace, policy, machine)
	} else {
		r.backup.lastSuccess.WithLabelValues(namespace, policy, machine).Set(float64(lastSuccess.Unix()))
	}
}
