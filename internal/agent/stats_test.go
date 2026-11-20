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
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestReconcileProjectsResourceUsageStats(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cpu_usage_percent": 17.5, "memory_usage_bytes": 2048, "disk_read_bytes": 10, "disk_write_bytes": 20,
			})
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
	if gotStatus.ResourceUsage == nil {
		t.Fatal("expected status.resourceUsage to be populated")
	}
	if gotStatus.ResourceUsage.CPUPercent != 17.5 || gotStatus.ResourceUsage.MemoryBytes != 2048 {
		t.Fatalf("unexpected resource usage: %+v", gotStatus.ResourceUsage)
	}
}

func TestReconcileToleratesStatsFailure(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1/stats":
			http.Error(w, "not supported", http.StatusNotFound)
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
		t.Fatalf("expected reconcile to tolerate a stats failure, got %v", err)
	}
	if gotStatus.Phase != "Running" {
		t.Fatalf("expected reconcile to still succeed overall, got phase %q", gotStatus.Phase)
	}
}
