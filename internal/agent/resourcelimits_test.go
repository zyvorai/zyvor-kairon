// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func uint32p(v uint32) *uint32 { return &v }
func uint64p(v uint64) *uint64 { return &v }

func TestReconcileResourceLimitsNilSpecNeverCallsFluxVM(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FluxVM should not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := model.Machine{Status: model.MachineStatus{AppliedResourceLimits: &model.ResourceLimits{CPUQuotaPercent: uint32p(100)}}}
	got, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got.CPUQuotaPercent != 100 {
		t.Fatalf("expected the previously-applied limits to be left untouched, got %+v", got)
	}
}

func TestReconcileResourceLimitsSkipsCallWhenUnchanged(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FluxVM should not be called when the desired limits already match status, got %s %s", r.Method, r.URL.Path)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	limits := &model.ResourceLimits{CPUQuotaPercent: uint32p(200)}
	m := model.Machine{
		Spec:   model.MachineSpec{Resources: model.ResourceSpec{Limits: limits}},
		Status: model.MachineStatus{AppliedResourceLimits: &model.ResourceLimits{CPUQuotaPercent: uint32p(200)}},
	}
	got, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got.CPUQuotaPercent != 200 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestReconcileResourceLimitsAppliesChange(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	desired := &model.ResourceLimits{CPUQuotaPercent: uint32p(300), MemoryMaxBytes: uint64p(2 << 30)}
	m := model.Machine{
		Spec:   model.MachineSpec{Resources: model.ResourceSpec{Limits: desired}},
		Status: model.MachineStatus{AppliedResourceLimits: &model.ResourceLimits{CPUQuotaPercent: uint32p(200)}},
	}
	got, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got.CPUQuotaPercent != *desired.CPUQuotaPercent || *got.MemoryMaxBytes != *desired.MemoryMaxBytes {
		t.Fatalf("expected the desired limits to be returned as applied, got %+v", got)
	}
	if gotPath != "/v1/vms/vm-1/resources" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	if gotBody["cpu_quota_percent"] != float64(300) || gotBody["memory_max_bytes"] != float64(2<<30) {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
}

func TestReconcileResourceLimitsAllowsLoweringUnlikeHotplug(t *testing.T) {
	var gotBody map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	desired := &model.ResourceLimits{CPUQuotaPercent: uint32p(50)}
	m := model.Machine{
		Spec:   model.MachineSpec{Resources: model.ResourceSpec{Limits: desired}},
		Status: model.MachineStatus{AppliedResourceLimits: &model.ResourceLimits{CPUQuotaPercent: uint32p(400)}},
	}
	if _, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	if gotBody["cpu_quota_percent"] != float64(50) {
		t.Fatalf("expected a lower cgroup limit to actually be applied (not ignored like hotplug shrink), got %+v", gotBody)
	}
}

func TestReconcileResourceLimitsPropagatesFluxVMFailure(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM has no cgroup (not running)", http.StatusBadRequest)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	desired := &model.ResourceLimits{CPUQuotaPercent: uint32p(300)}
	m := model.Machine{
		Spec:   model.MachineSpec{Resources: model.ResourceSpec{Limits: desired}},
		Status: model.MachineStatus{AppliedResourceLimits: &model.ResourceLimits{CPUQuotaPercent: uint32p(200)}},
	}
	got, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"})
	if err == nil {
		t.Fatal("expected an error to propagate")
	}
	if got == nil || *got.CPUQuotaPercent != 200 {
		t.Fatalf("expected status.appliedResourceLimits to stay at the last successfully-applied value on failure, got %+v", got)
	}
}

// TestReconcileResourceLimitsMergesAllocatedCPUSetWithSpecLimits proves the
// scheduler's own spec.resources.allocatedCpuSet decision and an
// independently-set spec.resources.limits both reach FluxVM in the exact
// same POST /v1/vms/{id}/resources call, not two separate/conflicting
// requests.
func TestReconcileResourceLimitsMergesAllocatedCPUSetWithSpecLimits(t *testing.T) {
	var gotBody map[string]any
	var callCount int
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := model.Machine{
		Spec: model.MachineSpec{Resources: model.ResourceSpec{
			Limits:          &model.ResourceLimits{CPUQuotaPercent: uint32p(300)},
			AllocatedCPUSet: []uint32{2, 3, 4},
		}},
	}
	got, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected exactly one FluxVM call, got %d", callCount)
	}
	if got == nil || *got.CPUQuotaPercent != 300 || len(got.CPUSetCPUs) != 3 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if gotBody["cpu_quota_percent"] != float64(300) {
		t.Fatalf("expected spec.resources.limits to still reach FluxVM, got %+v", gotBody)
	}
	cpus, _ := gotBody["cpuset_cpus"].([]any)
	if len(cpus) != 3 {
		t.Fatalf("expected spec.resources.allocatedCpuSet to reach FluxVM as cpuset_cpus, got %+v", gotBody["cpuset_cpus"])
	}
}

// TestReconcileResourceLimitsAllocatedCPUSetAloneStillCallsFluxVM proves a
// Machine with only spec.resources.cpuPinning (no spec.resources.limits at
// all) still applies its cpuset via this same path.
func TestReconcileResourceLimitsAllocatedCPUSetAloneStillCallsFluxVM(t *testing.T) {
	var gotBody map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := model.Machine{
		Spec: model.MachineSpec{Resources: model.ResourceSpec{AllocatedCPUSet: []uint32{5, 6}}},
	}
	if _, err := a.reconcileResourceLimits(context.Background(), m, &fluxvm.Record{IDValue: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	cpus, _ := gotBody["cpuset_cpus"].([]any)
	if len(cpus) != 2 {
		t.Fatalf("expected cpuset_cpus to reach FluxVM even with no spec.resources.limits set, got %+v", gotBody)
	}
}
