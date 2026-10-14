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
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// statusPatches records every status patch body sent for prod/db.
type statusPatches struct {
	mu   sync.Mutex
	list []map[string]any
}

func (p *statusPatches) last(t *testing.T) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.list) == 0 {
		t.Fatal("no status patch")
	}
	return p.list[len(p.list)-1]
}

func (p *statusPatches) any(key string) (any, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.list {
		if v, ok := s[key]; ok {
			return v, true
		}
	}
	return nil, false
}

func restartKube(t *testing.T, m model.Machine, patches *statusPatches) *kube.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{m}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/db/status":
			var body struct {
				Status map[string]any `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			patches.mu.Lock()
			patches.list = append(patches.list, body.Status)
			patches.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch, r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	return kc
}

type fakeFlux struct {
	mu     sync.Mutex
	status string
	calls  []string
}

func (f *fakeFlux) called(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}

func (f *fakeFlux) client(t *testing.T) *fluxvm.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: f.status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/stop":
			f.status = "stopped"
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: f.status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/start":
			f.status = "running"
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: f.status})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/qga/exec":
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	fc := fluxvm.New(srv.URL, "")
	fc.HTTP = srv.Client()
	return fc
}

func restartAgent(kc *kube.Client, fc *fluxvm.Client) *Agent {
	return &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestReconcilePowerCyclesOnRestartRequest(t *testing.T) {
	m := pausedMachine("Running")
	m.Status.Phase = "Running"
	m.Metadata.Annotations = map[string]string{model.AnnotationRestartRequested: "2026-10-14T06:00:00Z"}
	var patches statusPatches
	ff := &fakeFlux{status: "running"}
	if err := restartAgent(restartKube(t, m, &patches), ff.client(t)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ff.called("POST /v1/vms/vm-1/stop") || !ff.called("POST /v1/vms/vm-1/start") {
		t.Fatalf("expected stop then start, calls %v", ff.calls)
	}
	st := patches.last(t)
	if st["lastRestartRequest"] != "2026-10-14T06:00:00Z" || st["phase"] != "Running" {
		t.Fatalf("status %v", st)
	}
}

func TestReconcileIgnoresHandledRestartRequest(t *testing.T) {
	m := pausedMachine("Running")
	m.Status.Phase = "Running"
	m.Metadata.Annotations = map[string]string{model.AnnotationRestartRequested: "2026-10-14T06:00:00Z"}
	m.Status.LastRestartRequest = "2026-10-14T06:00:00Z"
	var patches statusPatches
	ff := &fakeFlux{status: "running"}
	if err := restartAgent(restartKube(t, m, &patches), ff.client(t)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ff.called("POST /v1/vms/vm-1/stop") {
		t.Fatalf("handled request restarted again: %v", ff.calls)
	}
}

func TestGracefulStopAsksTheGuestFirst(t *testing.T) {
	m := pausedMachine("Stopped")
	m.Status.Phase = "Running"
	m.Spec.GuestAgent.Enabled = true
	m.Metadata.Annotations = map[string]string{model.AnnotationStopGracePeriod: "30"}
	var patches statusPatches
	ff := &fakeFlux{status: "running"}
	if err := restartAgent(restartKube(t, m, &patches), ff.client(t)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ff.called("POST /v1/vms/vm-1/qga/exec") || ff.called("DELETE /v1/vms/vm-1") {
		t.Fatalf("calls %v: want a guest poweroff and no delete yet", ff.calls)
	}
	st := patches.last(t)
	deadline, err := time.Parse(time.RFC3339, st["shutdownDeadline"].(string))
	if err != nil || time.Until(deadline) < 20*time.Second {
		t.Fatalf("shutdownDeadline %v", st["shutdownDeadline"])
	}
}

func TestGracefulStopStopsAtTheDeadline(t *testing.T) {
	m := pausedMachine("Stopped")
	m.Status.Phase = "Running"
	m.Spec.GuestAgent.Enabled = true
	m.Metadata.Annotations = map[string]string{model.AnnotationStopGracePeriod: "30"}
	m.Status.ShutdownDeadline = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	var patches statusPatches
	ff := &fakeFlux{status: "running"}
	if err := restartAgent(restartKube(t, m, &patches), ff.client(t)).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ff.called("DELETE /v1/vms/vm-1") || ff.called("POST /v1/vms/vm-1/qga/exec") {
		t.Fatalf("calls %v: want a delete without asking the guest again", ff.calls)
	}
	if v, ok := patches.any("shutdownDeadline"); !ok || v != nil {
		t.Fatalf("shutdownDeadline not cleared with null: %v %v", v, ok)
	}
}

func TestStopGracePeriod(t *testing.T) {
	m := pausedMachine("Stopped")
	m.Metadata.Annotations = map[string]string{model.AnnotationStopGracePeriod: "30"}
	if got := stopGracePeriod(m); got != 0 {
		t.Fatalf("no guest agent: %s", got)
	}
	m.Spec.GuestAgent.Enabled = true
	if got := stopGracePeriod(m); got != 30*time.Second {
		t.Fatalf("got %s", got)
	}
	m.Metadata.Annotations[model.AnnotationStopGracePeriod] = "999999"
	if got := stopGracePeriod(m); got != maxStopGracePeriod {
		t.Fatalf("not capped: %s", got)
	}
	m.Metadata.Annotations[model.AnnotationStopGracePeriod] = "soon"
	if got := stopGracePeriod(m); got != 0 {
		t.Fatalf("invalid value: %s", got)
	}
}
