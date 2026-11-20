// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/zyvorai/kairon/internal/agent"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// flakyFluxVM answers every request with 500 while down is true (simulating
// a FluxVM process mid-restart/unreachable), and normally otherwise. Unlike
// fluxVMRunning above, this covers *both* of Agent.current's lookup paths
// (by runtime ID and by name) failing together, which is the realistic
// shape of "FluxVM itself is briefly unavailable" -- a fault that only
// takes out one of the two would already be masked by current()'s own
// fallback from ID-lookup to name-lookup.
type flakyFluxVM struct {
	down atomic.Bool
	id   string
	name string
}

func (f *flakyFluxVM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		http.Error(w, "injected fault: fluxvm unreachable", http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/"+f.id:
		_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: f.id, Name: f.name, Status: "Running"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == f.name:
		_ = json.NewEncoder(w).Encode([]fluxvm.Record{{UUID: f.id, Name: f.name, Status: "Running"}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// TestAgentRecoversFromTransientFluxVMOutage proves that a Machine whose
// node-local FluxVM instance goes briefly unreachable surfaces an honest
// "Error" status instead of silently freezing on stale data or crashing
// kairon-node -- and recovers to "Running" on its own the next tick once
// FluxVM answers again, with no operator action needed. This is the
// node-side half of "failure injection": internal/controller's own
// NeedsRecovery tests (internal/integration/migration_pipeline_test.go)
// already cover the equivalent failure during a live migration.
func TestAgentRecoversFromTransientFluxVMOutage(t *testing.T) {
	cluster := &fakeCluster{
		machine: model.Machine{
			Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
			Spec: model.MachineSpec{
				NodeName: "worker-1", PowerState: "Running",
				Image: model.ImageSpec{Path: "/images/db.qcow2"}, Runtime: model.RuntimeSpec{Backend: "qemu"},
				Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			},
			Status: model.MachineStatus{NodeName: "worker-1", Phase: "Running", RuntimeID: "vm-1"},
		},
		nodes: []model.Node{readyCapableNode("worker-1")},
	}
	ks := httptest.NewServer(cluster.handler())
	defer ks.Close()

	flux := &flakyFluxVM{id: "vm-1", name: "kairon-prod-db"}
	fluxServer := httptest.NewServer(flux)
	defer fluxServer.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fluxServer.URL, "")
	fc.HTTP = fluxServer.Client()
	ag := &agent.Agent{NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu", Log: log}

	// Baseline: FluxVM healthy, Machine stays Running.
	if err := ag.Reconcile(context.Background()); err != nil {
		t.Fatalf("baseline reconcile: %v", err)
	}
	if cluster.machine.Status.Phase != "Running" {
		t.Fatalf("baseline: expected Running, got %q", cluster.machine.Status.Phase)
	}

	// Inject the outage.
	flux.down.Store(true)
	if err := ag.Reconcile(context.Background()); err != nil {
		t.Fatalf("outage reconcile: %v", err)
	}
	if cluster.machine.Status.Phase != "Error" {
		t.Fatalf("during outage: expected Error, got %q message=%q", cluster.machine.Status.Phase, cluster.machine.Status.Message)
	}
	if cluster.machine.Status.Message == "" {
		t.Fatal("during outage: expected a non-empty status.message explaining the failure")
	}
	foundNotReady := false
	for _, c := range cluster.machine.Status.Conditions {
		if c.Type == "Ready" && c.Status == "False" {
			foundNotReady = true
		}
	}
	if !foundNotReady {
		t.Fatalf("during outage: expected a Ready=False condition, got %+v", cluster.machine.Status.Conditions)
	}

	// Recovery: FluxVM answers again on the very next tick, no operator
	// action taken in between.
	flux.down.Store(false)
	if err := ag.Reconcile(context.Background()); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	if cluster.machine.Status.Phase != "Running" {
		t.Fatalf("after recovery: expected Running, got %q message=%q", cluster.machine.Status.Phase, cluster.machine.Status.Message)
	}
	if cluster.machine.Status.Message != "" {
		t.Fatalf("after recovery: expected status.message cleared, got %q", cluster.machine.Status.Message)
	}
}
