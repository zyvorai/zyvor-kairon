// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleRuntimeCapabilitiesFullRelay(t *testing.T) {
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/capabilities" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"runtime.fluxvm.zyvor.io/v1","migration":[],"snapshot":[]}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin -- listing is any-operator
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/worker-1/capabilities", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlePressureFullRelay(t *testing.T) {
	var gotPath string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/runtime-1/pressure" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"cpu_some":{"avg10":1.2}}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin -- reading is any-operator
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/pressure", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/vms/runtime-1/pressure" {
		t.Fatalf("unexpected upstream path: %q", gotPath)
	}
}

func TestHandleFreezeDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/freeze", token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleFreezeThawFullRelay(t *testing.T) {
	var calls []string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/freeze", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("freeze: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/thaw", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("thaw: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(calls) != 2 || calls[0] != "/v1/vms/runtime-1/freeze" || calls[1] != "/v1/vms/runtime-1/thaw" {
		t.Fatalf("unexpected upstream calls: %v", calls)
	}
}
