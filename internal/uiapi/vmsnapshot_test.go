// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleVMSnapshotDeniesNonAdmin(t *testing.T) {
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-snapshot", token, vmSnapshotRequest{Tag: "before-upgrade"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleVMSnapshotDeniesWhenNotEnabled(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		// ConsoleToken/ConsolePort left empty
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-snapshot", token, vmSnapshotRequest{Tag: "t"})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleVMSnapshotDeniesWhenStopped(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Stopped", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-snapshot", token, vmSnapshotRequest{Tag: "t"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a Stopped machine, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleVMSnapshotAllowsPausedMachine(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/runtime-1/snapshot" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer fluxSrv.Close()

	const relayToken = "shared-node-relay-token"
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: relayToken}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	defer nodeSrv.Close()
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Paused", NodeName: "worker-1", RuntimeID: "runtime-1"},
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-snapshot", token, vmSnapshotRequest{Tag: "before-upgrade"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for a Paused machine, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleVMRestoreSnapshotFullRelay exercises the entire chain:
// kairon-ui's handleVMRestoreSnapshot -> a real internal/consoleproxy.Server
// (standing in for kairon-node) -> fluxvm.Client.RestoreSnapshot's own
// stop -> start-from-snapshot orchestration against a fake FluxVM server.
func TestHandleVMRestoreSnapshotFullRelay(t *testing.T) {
	var calls []string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/runtime-1/stop":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "runtime-1", Status: "Stopped"})
		case "/v1/vms/runtime-1/start-from-snapshot":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "runtime-1", Status: "Running"})
		default:
			http.Error(w, "bad route", http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()

	const relayToken = "shared-node-relay-token"
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: relayToken}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	defer nodeSrv.Close()
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-restore-snapshot", token, vmSnapshotRequest{Tag: "before-upgrade"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out vmSnapshotResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "Running" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if len(calls) != 2 || calls[0] != "POST /v1/vms/runtime-1/stop" || calls[1] != "POST /v1/vms/runtime-1/start-from-snapshot" {
		t.Fatalf("expected stop then start-from-snapshot in order, got %v", calls)
	}
}

func TestHandleVMRestoreSnapshotPropagatesNodeRelayFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such snapshot", http.StatusNotFound)
	}))
	defer fluxSrv.Close()

	const relayToken = "shared-node-relay-token"
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: relayToken}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	defer nodeSrv.Close()
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err := net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}

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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-restore-snapshot", token, vmSnapshotRequest{Tag: "missing-tag"})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the request, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleVMSnapshotRequiresTag(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/vm-snapshot", token, vmSnapshotRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a tag, got %d: %s", rr.Code, rr.Body.String())
	}
}
