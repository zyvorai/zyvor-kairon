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

// TestReconcileSelfHealsAFluxVMStoppedRuntime covers the safety-net half
// of reconcileMachine's Start branch: fluxvm.Client.RestoreSnapshot's own
// doc comment describes a stop->start-from-snapshot orchestration
// interrupted after the stop succeeds, leaving FluxVM reporting the
// runtime Stopped while spec.powerState still wants Running. Reconcile
// must notice the mismatch and call Start (not start-from-snapshot --
// this recovers to the VM's last-known-good disk state, it never
// re-attempts the restore itself). The exact same branch also handles
// the *intended* Halted -> Running resume path -- see
// TestReconcileResumesFromHaltedWhenPowerStateIsRunning below.
func TestReconcileSelfHealsAFluxVMStoppedRuntime(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var startCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			status := "Stopped"
			if startCalled {
				status = "Running"
			}
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
			startCalled = true
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
	if !startCalled {
		t.Fatal("expected Start to be called against FluxVM to recover a Stopped runtime")
	}
	if gotStatus.Phase != "Running" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestReconcileRestartsAFailedRuntimeUnlessAMigrationIsMovingIt(t *testing.T) {
	migration := func(strategy, phase, target string) model.MachineMigration {
		return model.MachineMigration{
			Metadata: model.ObjectMeta{Name: "mig", Namespace: "prod"},
			Spec:     model.MachineMigrationSpec{MachineName: "db", Strategy: strategy, TargetNode: target},
			Status:   model.MachineMigrationStatus{Phase: phase, EffectiveStrategy: strategy, TargetNode: target},
		}
	}
	for _, tc := range []struct {
		name       string
		migrations []model.MachineMigration
		wantStart  bool
	}{
		{"no migration CRD", nil, true},
		{"no migration", []model.MachineMigration{}, true},
		{"finished migration", []model.MachineMigration{migration("live", "Succeeded", "worker-2")}, true},
		{"cancelled migration", []model.MachineMigration{migration("live", "Cancelled", "worker-2")}, true},
		{"cold restart onto this node", []model.MachineMigration{migration("cold", "Restarting", "worker-1")}, true},
		{"live migration in flight", []model.MachineMigration{migration("live", "Running", "worker-2")}, false},
		{"post-copy needs recovery", []model.MachineMigration{migration("live", "NeedsRecovery", "worker-1")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := pausedMachine("Running")
			var gotStatus model.MachineStatus
			ks := fakeKubeForMachineWithMigrations(t, &m, &gotStatus, tc.migrations)
			defer ks.Close()

			var startCalled bool
			fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
					status := "failed"
					if startCalled {
						status = "running"
					}
					_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: status, Error: "writing shared disk lock: Read-only file system"})
				case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
					startCalled = true
					_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "running"})
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
			if startCalled != tc.wantStart {
				t.Fatalf("Start called = %v, want %v", startCalled, tc.wantStart)
			}
			wantPhase := "Unknown"
			if tc.wantStart {
				wantPhase = "Running"
			}
			if gotStatus.Phase != wantPhase {
				t.Fatalf("phase = %q, want %q", gotStatus.Phase, wantPhase)
			}
		})
	}
}

func TestReconcileDeletesAFailedRuntimeWhoseDiskIsGone(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var deleted bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "failed"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
			http.Error(w, `{"error":"cannot start vm-1: disk no longer exists at /var/lib/fluxvm/instances/vm-1/root.qcow2"}`, http.StatusBadRequest)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-1":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
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
	if !deleted {
		t.Fatal("expected the failed runtime with no disk to be deleted so the next tick recreates it")
	}
	if gotStatus.Phase != "Error" {
		t.Fatalf("phase = %q, want Error until the runtime is recreated", gotStatus.Phase)
	}
}

// TestReconcileNeverStartsAnIntentionallyStoppedMachine is the regression
// guard on the branch's own DesiredPowerState() != "Stopped" condition:
// spec.powerState: Stopped goes through ensureStopped's own Delete path
// entirely, never this one -- a Machine with no runtime left has no
// "/v1/vms/vm-1" route to even answer a GET, so any accidental Start call
// here would hit a 404 the fake server turns into a fatal test failure.
func TestReconcileNeverStartsAnIntentionallyStoppedMachine(t *testing.T) {
	m := pausedMachine("Stopped")
	m.Status.RuntimeID = ""
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == "kairon-prod-db":
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
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
	if gotStatus.Phase != "Stopped" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestEnsureHaltedStopsARunningMachine(t *testing.T) {
	m := pausedMachine("Halted")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var stopCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/stop":
			stopCalled = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Stopped"})
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
	if !stopCalled {
		t.Fatal("expected Stop to be called against FluxVM")
	}
	// Deliberately not "Stopped" -- see ensureHalted's own doc comment on
	// why Halted needs a distinct Machine-level phase from the full
	// teardown ensureStopped reports.
	if gotStatus.Phase != "Halted" || gotStatus.RuntimeID != "vm-1" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestEnsureHaltedIsANoOpWhenAlreadyStopped(t *testing.T) {
	m := pausedMachine("Halted")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var stopCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Stopped"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/stop":
			stopCalled = true
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Stopped"})
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
	if stopCalled {
		t.Fatal("expected no Stop call when the runtime already reports Stopped")
	}
	if gotStatus.Phase != "Halted" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}

func TestEnsureHaltedWithNoRuntimeReportsPending(t *testing.T) {
	m := pausedMachine("Halted")
	m.Status.RuntimeID = "" // never created
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == "kairon-prod-db":
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
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
	if gotStatus.Phase != "Pending" || gotStatus.Message == "" {
		t.Fatalf("expected a Pending status with an explanatory message, got %+v", gotStatus)
	}
}

func TestReconcileResumesFromHaltedWhenPowerStateIsRunning(t *testing.T) {
	m := pausedMachine("Running")
	var gotStatus model.MachineStatus
	ks := fakeKubeForMachine(t, &m, &gotStatus)
	defer ks.Close()

	var startCalled bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			status := "Stopped"
			if startCalled {
				status = "Running"
			}
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
			startCalled = true
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
	if !startCalled {
		t.Fatal("expected Start to be called against FluxVM to resume a Halted machine")
	}
	if gotStatus.Phase != "Running" {
		t.Fatalf("unexpected status: %+v", gotStatus)
	}
}
