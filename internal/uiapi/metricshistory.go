// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// metricsHistoryCap bounds each Machine's ring: 240 samples at the 15 s
// sampler interval is one hour.
const metricsHistoryCap = 240

// usageSample is one point of a Machine's usage history. CPUPercent is the
// same lifetime-average percentage Machine.status.resourceUsage reports
// (see model.ResourceUsage), so a chart shows the trend of that value, not an
// instantaneous rate.
type usageSample struct {
	Time           time.Time `json:"time"`
	CPUPercent     float64   `json:"cpuPercent"`
	MemoryBytes    uint64    `json:"memoryBytes"`
	DiskReadBytes  uint64    `json:"diskReadBytes"`
	DiskWriteBytes uint64    `json:"diskWriteBytes"`
}

// metricsHistory is an in-memory per-Machine ring buffer filled by
// RunMetricsSampler. It needs no Prometheus and no storage, at the cost of
// history that is per-replica and lost on restart.
type metricsHistory struct {
	mu     sync.Mutex
	series map[string][]usageSample
}

func newMetricsHistory() *metricsHistory {
	return &metricsHistory{series: map[string][]usageSample{}}
}

func historyKey(ns, name string) string { return ns + "/" + name }

// record appends one sample per Machine that reports usage and drops the
// series of Machines that no longer exist.
func (h *metricsHistory) record(machines []model.Machine, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	live := make(map[string]bool, len(machines))
	for _, m := range machines {
		k := historyKey(m.Namespace(), m.Metadata.Name)
		live[k] = true
		u := m.Status.ResourceUsage
		if u == nil {
			continue
		}
		series := h.series[k]
		series = append(series, usageSample{
			Time: now, CPUPercent: u.CPUPercent, MemoryBytes: u.MemoryBytes,
			DiskReadBytes: u.DiskReadBytes, DiskWriteBytes: u.DiskWriteBytes,
		})
		if len(series) > metricsHistoryCap {
			series = append([]usageSample(nil), series[len(series)-metricsHistoryCap:]...)
		}
		h.series[k] = series
	}
	for k := range h.series {
		if !live[k] {
			delete(h.series, k)
		}
	}
}

// query returns a copy of the samples at or after since.
func (h *metricsHistory) query(ns, name string, since time.Time) []usageSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []usageSample{}
	for _, s := range h.series[historyKey(ns, name)] {
		if !s.Time.Before(since) {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) usageHistory() *metricsHistory {
	s.historyOnce.Do(func() { s.history = newMetricsHistory() })
	return s.history
}

// RunMetricsSampler lists Machines every interval and records their usage
// until ctx ends. cmd/kairon-ui starts it in its own goroutine.
func (s *Server) RunMetricsSampler(ctx context.Context, interval time.Duration) {
	sample := func() {
		machines, err := s.Kube.ListMachines(ctx)
		if err != nil {
			if s.Log != nil && ctx.Err() == nil {
				s.Log.Warn("uiapi metrics sampler list failed", "error", err)
			}
			return
		}
		s.usageHistory().record(machines, time.Now())
	}
	sample()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sample()
		}
	}
}

// handleMachineMetrics serves GET /api/v1/machines/{ns}/{name}/metrics
// ?range=15m (default 15m, max 1h): the recorded usage samples, oldest first.
func (s *Server) handleMachineMetrics(w http.ResponseWriter, r *http.Request) {
	window := 15 * time.Minute
	if v := r.URL.Query().Get("range"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "range must be a positive duration such as 15m")
			return
		}
		window = min(d, time.Hour)
	}
	samples := s.usageHistory().query(r.PathValue("namespace"), r.PathValue("name"), time.Now().Add(-window))
	writeJSON(w, http.StatusOK, map[string]any{
		"intervalSeconds": 15,
		"samples":         samples,
	})
}
