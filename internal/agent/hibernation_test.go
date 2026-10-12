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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type hibernationFake struct {
	mu        sync.Mutex
	calls     []string
	status    *model.MachineStatus
	vms       []map[string]any
	machines  []model.Machine
	importErr bool
}

func (f *hibernationFake) called(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			return true
		}
	}
	return false
}

func hibernatedRecord(id string) map[string]any {
	return map[string]any{
		"id": id, "name": "kairon-prod-vm-1", "status": "stopped",
		"hibernation": map[string]any{
			"dir": "/var/lib/fluxvm/hibernation/" + id,
			"manifest": map[string]any{
				"format": 1, "vm_id": id, "hibernated_at": "2026-10-12T06:31:00Z", "node": "n1",
				"host":  map[string]any{"qemu_version": "10.2.1", "machine_type": "pc-q35-10.2", "cpu_vendor": "GenuineIntel", "cpu_flags": []string{"avx2"}, "state_dir": "/var/lib/fluxvm/hibernation"},
				"vcpus": 1, "memory_mib": 1024, "state_bytes": 467073591, "shared_storage": false,
			},
		},
	}
}

func newHibernationAgent(t *testing.T, fake *hibernationFake) *Agent {
	t.Helper()
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.calls = append(fake.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/rt-1":
			for _, vm := range fake.vms {
				if vm["id"] == "rt-1" {
					_ = json.NewEncoder(w).Encode(vm)
					return
				}
			}
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			if r.URL.Query().Get("name") != "" {
				_, _ = w.Write([]byte("[]"))
				return
			}
			_ = json.NewEncoder(w).Encode(fake.vms)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/rt-1/hibernate":
			_ = json.NewEncoder(w).Encode(hibernatedRecord("rt-1"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/import-hibernated":
			if fake.importErr {
				http.Error(w, `{"error":"hibernated state is not compatible with this host: QEMU 9.2.0 saved the state"}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(hibernatedRecord("rt-1"))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/rt-old":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(fs.Close)
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			fake.status = &p.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: fake.machines})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func machineVM1(power string) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "n1", PowerState: power},
	}
	m.Status.RuntimeID = "rt-1"
	m.Status.Phase = "Running"
	return m
}

func TestEnsureHibernatedSavesStateAndReportsIt(t *testing.T) {
	fake := &hibernationFake{vms: []map[string]any{{"id": "rt-1", "name": "kairon-prod-vm-1", "status": "running"}}}
	a := newHibernationAgent(t, fake)
	if err := a.ensureHibernated(context.Background(), machineVM1(model.PowerStateHibernated)); err != nil {
		t.Fatal(err)
	}
	if !fake.called("POST /v1/vms/rt-1/hibernate") {
		t.Fatalf("no hibernate call: %v", fake.calls)
	}
	st := fake.status
	if st == nil || st.Phase != model.PowerStateHibernated || st.Hibernation == nil {
		t.Fatalf("status %+v", st)
	}
	h := st.Hibernation
	if h.Node != "n1" || h.RuntimeID != "rt-1" || h.StateBytes != 467073591 || h.Host.QEMUVersion != "10.2.1" || h.SharedStorage {
		t.Fatalf("hibernation status %+v", h)
	}
}

func TestEnsureHibernatedRefusesPoweredOffRuntime(t *testing.T) {
	fake := &hibernationFake{vms: []map[string]any{{"id": "rt-1", "name": "kairon-prod-vm-1", "status": "stopped"}}}
	a := newHibernationAgent(t, fake)
	if err := a.ensureHibernated(context.Background(), machineVM1(model.PowerStateHibernated)); err != nil {
		t.Fatal(err)
	}
	if fake.called("POST /v1/vms/rt-1/hibernate") {
		t.Fatal("hibernated a stopped runtime")
	}
	if fake.status == nil || !strings.Contains(fake.status.Message, "powered-off") {
		t.Fatalf("status %+v", fake.status)
	}
}

func TestResumeGateWaitsForTheController(t *testing.T) {
	fake := &hibernationFake{}
	a := newHibernationAgent(t, fake)
	m := machineVM1("Running")
	m.Status.Hibernation = &model.HibernationStatus{Node: "n1", RuntimeID: "rt-1"}
	rec := &fluxvm.Record{IDValue: "rt-1", Status: "stopped", Hibernation: &fluxvm.Hibernation{}}
	_, handled, err := a.resumeGate(context.Background(), m, rec)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if fake.status == nil || fake.status.Phase != model.PowerStateHibernated {
		t.Fatalf("status %+v", fake.status)
	}
	m.Status.Hibernation.ResumeNode = "n1"
	got, handled, err := a.resumeGate(context.Background(), m, rec)
	if err != nil || handled || got != rec {
		t.Fatalf("resume node set: handled=%v err=%v", handled, err)
	}
}

func TestResumeGateImportsFromAnotherNode(t *testing.T) {
	fake := &hibernationFake{}
	a := newHibernationAgent(t, fake)
	m := machineVM1("Running")
	m.Status.Hibernation = &model.HibernationStatus{Node: "n0", RuntimeID: "rt-1", ResumeNode: "n1", SharedStorage: true}
	got, handled, err := a.resumeGate(context.Background(), m, nil)
	if err != nil || handled || got == nil || got.ID() != "rt-1" {
		t.Fatalf("got=%v handled=%v err=%v", got, handled, err)
	}
	fake.importErr = true
	_, handled, err = a.resumeGate(context.Background(), m, nil)
	if err != nil || !handled {
		t.Fatalf("failed import: handled=%v err=%v", handled, err)
	}
	if fake.status == nil || fake.status.Phase != "Blocked" || !strings.Contains(fake.status.Message, model.AnnotationDiscardHibernation) {
		t.Fatalf("status %+v", fake.status)
	}
}

func TestResumeGatePassesResumedRuntime(t *testing.T) {
	a := newHibernationAgent(t, &hibernationFake{})
	m := machineVM1("Running")
	m.Status.Hibernation = &model.HibernationStatus{Node: "n1", RuntimeID: "rt-1"}
	rec := &fluxvm.Record{IDValue: "rt-1", Status: "running"}
	got, handled, err := a.resumeGate(context.Background(), m, rec)
	if err != nil || handled || got != rec {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
}

func TestSweepDeletesOnlyRuntimesNoMachineHereOwns(t *testing.T) {
	moved := machineVM1("Running")
	moved.Spec.NodeName = "n2"
	old := hibernatedRecord("rt-old")
	waiting := hibernatedRecord("rt-1")
	waiting["name"] = "kairon-prod-vm-2"
	pending := machineVM1("Running")
	pending.Metadata.Name = "vm-2"
	pending.Spec.NodeName = "n2"
	pending.Status.Hibernation = &model.HibernationStatus{Node: "n1", RuntimeID: "rt-1"}
	foreign := hibernatedRecord("rt-x")
	foreign["name"] = "someone-elses-vm"
	fake := &hibernationFake{vms: []map[string]any{old, waiting, foreign}, machines: []model.Machine{moved, pending}}
	a := newHibernationAgent(t, fake)
	a.sweepMovedHibernations(context.Background())
	if len(fake.calls) != 0 {
		t.Fatalf("first tick swept: %v", fake.calls)
	}
	a.hibernationSweptAt = time.Now().Add(-2 * hibernationSweepEvery)
	a.sweepMovedHibernations(context.Background())
	if !fake.called("DELETE /v1/vms/rt-old") {
		t.Fatalf("moved-away runtime kept: %v", fake.calls)
	}
	if fake.called("DELETE /v1/vms/rt-1") || fake.called("DELETE /v1/vms/rt-x") {
		t.Fatalf("deleted a runtime still needed or not Kairon's: %v", fake.calls)
	}
	fake.calls = nil
	a.sweepMovedHibernations(context.Background())
	if len(fake.calls) != 0 {
		t.Fatalf("sweep not throttled: %v", fake.calls)
	}
}

func TestHibernationMovedAway(t *testing.T) {
	m := machineVM1("Running")
	if hibernationMovedAway(m, "n1", "rt-1") {
		t.Fatal("still assigned here")
	}
	m.Spec.NodeName = "n2"
	m.Status.Hibernation = &model.HibernationStatus{RuntimeID: "rt-1"}
	if hibernationMovedAway(m, "n1", "rt-1") {
		t.Fatal("not imported yet")
	}
	m.Status.Hibernation = nil
	if !hibernationMovedAway(m, "n1", "rt-1") {
		t.Fatal("resumed elsewhere")
	}
}
