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

func TestHandleAgentExecDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Console: true}},
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-exec", token, agentExecRequest{Command: "echo hi"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAgentExecDeniesWhenGuestAgentConsoleNotEnabled(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.GuestAgent.Console left false
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-exec", token, agentExecRequest{Command: "echo hi"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when spec.guestAgent.console is false, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAgentExecFullRelay exercises the entire chain:
// kairon-ui's handleAgentExec -> a real internal/consoleproxy.Server
// (standing in for kairon-node) -> a fake FluxVM server answering
// POST /v1/vms/{id}/agent.
func TestHandleAgentExecFullRelay(t *testing.T) {
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/runtime-1/agent":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "exec", "exit_code": 0, "stdout": "hello\n", "stderr": ""})
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
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Console: true}},
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-exec", token, agentExecRequest{Command: "echo hello"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out agentExecResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if gotBody["command"] != "echo hello" {
		t.Fatalf("expected the request to reach FluxVM through both relay hops, got %+v", gotBody)
	}
}

func TestHandleAgentExecPropagatesNodeRelayFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "vsock agent unreachable", http.StatusBadGateway)
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
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Console: true}},
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-exec", token, agentExecRequest{Command: "echo hi"})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the request, got %d: %s", rr.Code, rr.Body.String())
	}
}
