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
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func hotplugMachine(cpu, memory string, appliedVCPUs uint32, appliedMemoryMiB uint64) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: cpu, Memory: memory}},
		Status:   model.MachineStatus{AppliedVCPUs: appliedVCPUs, AppliedMemoryMiB: appliedMemoryMiB},
	}
}

// Three cases where reconcileHotplug must never call FluxVM at all, each
// expected to just report the target spec as already-applied: a fresh
// creation (nothing to hotplug yet, this *is* the baseline), no recorded
// baseline (an adopted Machine or a pre-hotplug-tracking kairon-node
// build), and a shrink request (FluxVM has no CPU/DIMM unplug).
func TestReconcileHotplugCasesThatNeverCallFluxVM(t *testing.T) {
	cases := []struct {
		name           string
		machine        model.Machine
		freshlyCreated bool
	}{
		{"freshly created seeds the baseline", hotplugMachine("2", "2Gi", 0, 0), true},
		{"no recorded baseline assumes already realized", hotplugMachine("2", "2Gi", 0, 0), false},
		{"a shrink request is ignored, applied stays put", hotplugMachine("1", "1Gi", 4, 4096), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("FluxVM should not be called, got %s %s", r.Method, r.URL.Path)
			}))
			defer fs.Close()
			fc := fluxvm.New(fs.URL, "")
			fc.HTTP = fs.Client()
			a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

			wantVCPUs, err := model.ParseVCPUs(tc.machine.Spec.Resources.CPU)
			if err != nil {
				t.Fatal(err)
			}
			wantMemMiB, err := model.ParseMemoryMiB(tc.machine.Spec.Resources.Memory)
			if err != nil {
				t.Fatal(err)
			}
			if tc.machine.Status.AppliedVCPUs != 0 || tc.machine.Status.AppliedMemoryMiB != 0 {
				// The shrink case: applied stays at its prior value, not the (smaller) target.
				wantVCPUs, wantMemMiB = tc.machine.Status.AppliedVCPUs, tc.machine.Status.AppliedMemoryMiB
			}

			vcpus, memMiB, err := a.reconcileHotplug(context.Background(), tc.machine, &fluxvm.Record{UUID: "vm-1"}, tc.freshlyCreated)
			if err != nil {
				t.Fatalf("reconcileHotplug: %v", err)
			}
			if vcpus != wantVCPUs || memMiB != wantMemMiB {
				t.Fatalf("got vcpus=%d memMiB=%d, want %d/%d", vcpus, memMiB, wantVCPUs, wantMemMiB)
			}
		})
	}
}

func TestReconcileHotplugGrowsCPUAndMemory(t *testing.T) {
	var cpuBody, memBody map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/cpu":
			_ = json.NewDecoder(r.Body).Decode(&cpuBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/memory":
			_ = json.NewDecoder(r.Body).Decode(&memBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"memory_mib": 4096})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := hotplugMachine("4", "4Gi", 2, 2048)
	vcpus, memMiB, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err != nil {
		t.Fatalf("reconcileHotplug: %v", err)
	}
	if vcpus != 4 || memMiB != 4096 {
		t.Fatalf("got vcpus=%d memMiB=%d, want 4/4096", vcpus, memMiB)
	}
	if cpuBody["add_vcpus"] != float64(2) {
		t.Fatalf("expected add_vcpus=2 (delta), got %v", cpuBody["add_vcpus"])
	}
	if memBody["add_memory_mib"] != float64(2048) {
		t.Fatalf("expected add_memory_mib=2048 (delta), got %v", memBody["add_memory_mib"])
	}
}

func TestReconcileHotplugDoesNotAdvanceAppliedOnFailure(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not enough hotplug headroom"}`, http.StatusBadRequest)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := hotplugMachine("8", "2Gi", 2, 2048)
	vcpus, memMiB, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err == nil || !strings.Contains(err.Error(), "hotplug cpu") {
		t.Fatalf("expected a hotplug cpu error, got %v", err)
	}
	if vcpus != 2 || memMiB != 2048 {
		t.Fatalf("expected applied totals unchanged on failure so the same delta retries next tick, got vcpus=%d memMiB=%d", vcpus, memMiB)
	}
}

// hotplugMachineOnNode is hotplugMachine plus a real spec.nodeName --
// quotaBlocksResize's own MachineCountsTowardQuota guard requires one
// (matching a real, already-scheduled Machine kairon-node would actually
// be hotplug-reconciling), so these quota-specific tests need it set,
// unlike hotplugMachine's own fixture above.
func hotplugMachineOnNode(cpu, memory string, appliedVCPUs uint32, appliedMemoryMiB uint64) model.Machine {
	m := hotplugMachine(cpu, memory, appliedVCPUs, appliedMemoryMiB)
	m.Spec.NodeName = "worker-1"
	return m
}

func kubeServerWithQuotas(t *testing.T, quotas []model.MachineQuota, machines []model.Machine) *kube.Client {
	t.Helper()
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinequotas":
			_ = json.NewEncoder(w).Encode(model.MachineQuotaList{Items: quotas})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: machines})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	return kc
}

func TestReconcileHotplugBlockedByMachineQuota(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FluxVM should not be called when the resize is blocked by quota, got %s %s", r.Method, r.URL.Path)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()

	m := hotplugMachineOnNode("8", "4Gi", 2, 2048) // requesting 8 vcpus, up from 2
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxTotalCPU: "4"}, // m's own target (8) alone already exceeds this
	}
	kc := kubeServerWithQuotas(t, []model.MachineQuota{quota}, []model.Machine{m})
	a := &Agent{Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	vcpus, memMiB, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err != nil {
		t.Fatalf("reconcileHotplug: %v", err)
	}
	if vcpus != 2 || memMiB != 2048 {
		t.Fatalf("expected applied totals to stay unchanged when blocked by quota, got vcpus=%d memMiB=%d", vcpus, memMiB)
	}
}

func TestReconcileHotplugAllowedWithinMachineQuota(t *testing.T) {
	var gotCPUCall bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/cpu":
			gotCPUCall = true
			_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()

	m := hotplugMachineOnNode("4", "2Gi", 2, 2048) // requesting 4 vcpus, up from 2
	quota := model.MachineQuota{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxTotalCPU: "8"}, // comfortably within
	}
	kc := kubeServerWithQuotas(t, []model.MachineQuota{quota}, []model.Machine{m})
	a := &Agent{Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	vcpus, _, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err != nil {
		t.Fatalf("reconcileHotplug: %v", err)
	}
	if vcpus != 4 {
		t.Fatalf("expected the resize to proceed (vcpus=4), got %d", vcpus)
	}
	if !gotCPUCall {
		t.Fatal("expected FluxVM's hotplug/cpu to actually be called")
	}
}

func TestReconcileHotplugProceedsWhenNoMachineQuotaInNamespace(t *testing.T) {
	var gotCPUCall bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCPUCall = true
		_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()

	m := hotplugMachineOnNode("4", "2Gi", 2, 2048)
	kc := kubeServerWithQuotas(t, nil, []model.Machine{m}) // no MachineQuota objects at all
	a := &Agent{Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	vcpus, _, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err != nil {
		t.Fatalf("reconcileHotplug: %v", err)
	}
	if vcpus != 4 || !gotCPUCall {
		t.Fatalf("expected the resize to proceed normally with no MachineQuota configured, got vcpus=%d called=%v", vcpus, gotCPUCall)
	}
}

func TestReconcileHotplugFailsOpenWhenQuotaLookupErrors(t *testing.T) {
	var gotCPUCall bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCPUCall = true
		_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()

	// A ServiceAccount without the machinequotas RBAC this check needs
	// (e.g. a binary upgraded ahead of its chart) sees this as a 403 --
	// must fail open (proceed with the resize, same as always) rather
	// than newly blocking hotplug on infrastructure this check itself
	// depends on.
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer ks.Close()
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	a := &Agent{Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := hotplugMachineOnNode("4", "2Gi", 2, 2048)
	vcpus, _, err := a.reconcileHotplug(context.Background(), m, &fluxvm.Record{UUID: "vm-1"}, false)
	if err != nil {
		t.Fatalf("reconcileHotplug: %v", err)
	}
	if vcpus != 4 || !gotCPUCall {
		t.Fatalf("expected the resize to proceed (fail open) when the quota lookup itself errors, got vcpus=%d called=%v", vcpus, gotCPUCall)
	}
}

func TestReconcileMachineEndToEndTracksAppliedResourcesAndHotplugs(t *testing.T) {
	machine := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
		Spec: model.MachineSpec{
			NodeName:   "worker-1",
			Image:      model.ImageSpec{Path: "/images/db.qcow2"},
			Resources:  model.ResourceSpec{CPU: "4", Memory: "4Gi"},
			Runtime:    model.RuntimeSpec{Backend: "qemu"},
			PowerState: "Running",
		},
		Status: model.MachineStatus{RuntimeID: "vm-1", AppliedVCPUs: 2, AppliedMemoryMiB: 2048},
	}
	var patchedStatus model.MachineStatus
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
		case r.Method == http.MethodGet && (strings.Contains(r.URL.Path, "networksecuritygroups") || strings.Contains(r.URL.Path, "machinenetworkpolicies") || strings.Contains(r.URL.Path, "machinemigrations")):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			patchedStatus = p.Status
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ks.Close()
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running", GuestIP: "10.0.0.5"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/cpu":
			_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/memory":
			_ = json.NewEncoder(w).Encode(map[string]any{"memory_mib": 4096})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patchedStatus.AppliedVCPUs != 4 || patchedStatus.AppliedMemoryMiB != 4096 {
		t.Fatalf("expected status to record the new hotplugged totals, got %+v", patchedStatus)
	}
}

func restartedRecord(vcpus uint32, memMiB uint64) *fluxvm.Record {
	rec := &fluxvm.Record{UUID: "vm-1"}
	rec.Request.VCPUs, rec.Request.MemoryMiB = vcpus, memMiB
	return rec
}

func TestReconcileHotplugAfterRestartReappliesPriorSize(t *testing.T) {
	var cpuBody, memBody map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/cpu":
			_ = json.NewDecoder(r.Body).Decode(&cpuBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"vcpus": 4})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/hotplug/memory":
			_ = json.NewDecoder(r.Body).Decode(&memBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"memory_mib": 4096})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// spec was never persisted (no annotation): prior applied 4/4096 is larger.
	m := hotplugMachine("2", "2Gi", 4, 4096)
	vcpus, memMiB, err := a.reconcileHotplugAfterRestart(context.Background(), m, restartedRecord(2, 2048))
	if err != nil {
		t.Fatalf("reconcileHotplugAfterRestart: %v", err)
	}
	if vcpus != 4 || memMiB != 4096 {
		t.Fatalf("got vcpus=%d memMiB=%d, want 4/4096", vcpus, memMiB)
	}
	if cpuBody["add_vcpus"] != float64(2) || memBody["add_memory_mib"] != float64(2048) {
		t.Fatalf("expected deltas from the boot size, got cpu=%v mem=%v", cpuBody, memBody)
	}
}

func TestReconcileHotplugAfterRestartReportsBootSizeOnFailure(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not enough hotplug headroom"}`, http.StatusBadRequest)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := hotplugMachine("4", "4Gi", 4, 4096)
	vcpus, memMiB, err := a.reconcileHotplugAfterRestart(context.Background(), m, restartedRecord(2, 2048))
	if err == nil {
		t.Fatal("expected a hotplug error")
	}
	if vcpus != 2 || memMiB != 2048 {
		t.Fatalf("status must report the real boot size after a failed re-hotplug, got vcpus=%d memMiB=%d", vcpus, memMiB)
	}
}

func TestReconcileHotplugAfterRestartAtFullSizeDoesNothing(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FluxVM should not be called, got %s %s", r.Method, r.URL.Path)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	m := hotplugMachine("4", "4Gi", 4, 4096)
	vcpus, memMiB, err := a.reconcileHotplugAfterRestart(context.Background(), m, restartedRecord(4, 4096))
	if err != nil || vcpus != 4 || memMiB != 4096 {
		t.Fatalf("got vcpus=%d memMiB=%d err=%v, want 4/4096", vcpus, memMiB, err)
	}
}
