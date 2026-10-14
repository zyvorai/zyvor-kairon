// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

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

func pausedMachine(powerState string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{model.Finalizer}},
		Spec: model.MachineSpec{
			NodeName:   "worker-1",
			Image:      model.ImageSpec{Path: "/images/db.qcow2"},
			Resources:  model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:    model.RuntimeSpec{Backend: "qemu"},
			PowerState: powerState,
		},
		Status: model.MachineStatus{NodeName: "worker-1", RuntimeID: "vm-1"},
	}
}

// fakeKubeForMachine mirrors the fake apiserver shape internal/agent's own
// TestReconcileCreatesFluxVMAndUpdatesStatus uses -- a single Machine,
// capturing whatever status gets patched onto it.
func fakeKubeForMachine(t *testing.T, m *model.Machine, gotStatus *model.MachineStatus) *httptest.Server {
	t.Helper()
	return fakeKubeForMachineWithMigrations(t, m, gotStatus, nil)
}

// fakeKubeForMachineWithMigrations also lists migrations in m's namespace
// (a nil slice answers 404, as an apiserver without the CRD would).
func fakeKubeForMachineWithMigrations(t *testing.T, m *model.Machine, gotStatus *model.MachineStatus, migrations []model.MachineMigration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{*m}})
		case migrations != nil && r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{Items: migrations})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			*gotStatus = p.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db":
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
}

func TestEnsurePausedPausesARunningMachine(t *testing.T) {
	m := pausedMachine("Paused")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var pauseCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/pause":
			pauseCalled = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Paused"})
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
	if !pauseCalled {
		t.Fatal("expected Pause to be called against FluxVM")
	}
	if gotStatus.Phase != "Paused" || gotStatus.RuntimeID != "vm-1" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestEnsurePausedIsANoOpWhenAlreadyPaused(t *testing.T) {
	m := pausedMachine("Paused")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var pauseCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Paused"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/pause":
			pauseCalled = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Paused"})
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
	if pauseCalled {
		t.Fatal("expected no Pause call when the runtime already reports Paused")
	}
	if gotStatus.Phase != "Paused" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestEnsurePausedWithNoRuntimeBootsThenPauses(t *testing.T) {
	m := pausedMachine("Paused")
	m.Status.RuntimeID = "" // never created: kaironctl start --paused
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var created, paused bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			recs := []fluxvm.Record{}
			if created {
				recs = append(recs, fluxvm.Record{UUID: "vm-9", Name: "kairon-prod-db", Status: "Running"})
			}
			_ = json.NewEncoder(w).Encode(recs)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			created = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-9", Name: "kairon-prod-db", Status: "Running"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-9":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-9", Name: "kairon-prod-db", Status: "Running"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-9/pause":
			paused = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-9", Status: "Paused"})
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
	if !created || !paused {
		t.Fatalf("created=%v paused=%v, want both", created, paused)
	}
	if gotStatus.Phase != "Paused" || gotStatus.RuntimeID != "vm-9" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestReconcileResumesFromPausedWhenPowerStateIsRunning(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var resumeCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			status := "Paused"
			if resumeCalled {
				status = "Running"
			}
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/resume":
			resumeCalled = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
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
	if !resumeCalled {
		t.Fatal("expected Resume to be called against FluxVM")
	}
	if gotStatus.Phase != "Running" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}
