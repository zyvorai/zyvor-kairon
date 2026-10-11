// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func usageMachine(name string, cpu float64) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "default"},
		Status:   model.MachineStatus{ResourceUsage: &model.ResourceUsage{CPUPercent: cpu, MemoryBytes: 1 << 20}},
	}
}

func TestMetricsHistoryRingAndPrune(t *testing.T) {
	h := newMetricsHistory()
	now := time.Now()
	for i := 0; i < metricsHistoryCap+10; i++ {
		h.record([]model.Machine{usageMachine("a", float64(i)), usageMachine("b", 1)}, now.Add(time.Duration(i)*time.Second))
	}
	got := h.query("default", "a", now.Add(-time.Hour))
	if len(got) != metricsHistoryCap {
		t.Fatalf("ring len = %d, want %d", len(got), metricsHistoryCap)
	}
	if got[0].CPUPercent != 10 || got[len(got)-1].CPUPercent != float64(metricsHistoryCap+9) {
		t.Errorf("ring kept wrong window: first=%v last=%v", got[0].CPUPercent, got[len(got)-1].CPUPercent)
	}
	h.record([]model.Machine{usageMachine("a", 1)}, now)
	if n := len(h.query("default", "b", now.Add(-time.Hour))); n != 0 {
		t.Errorf("deleted machine kept %d samples", n)
	}
}

func TestMachineMetricsEndpoint(t *testing.T) {
	fk := newFakeKube()
	s := newTestServer(t, fk, "")
	s.usageHistory().record([]model.Machine{usageMachine("vm-1", 42)}, time.Now())
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm-1/metrics?range=5m", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Samples []usageSample `json:"samples"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Samples) != 1 || got.Samples[0].CPUPercent != 42 {
		t.Fatalf("samples = %+v", got.Samples)
	}
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm-1/metrics?range=banana", "", nil); rr.Code != http.StatusBadRequest {
		t.Errorf("bad range: status %d", rr.Code)
	}
}
