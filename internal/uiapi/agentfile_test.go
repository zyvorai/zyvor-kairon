// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleAgentPutFileDeniesNonAdmin(t *testing.T) {
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/put", token, agentPutFileRequest{Path: "/etc/x", ContentBase64: "aGVsbG8="})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAgentFileDeniesWhenNotEnabled(t *testing.T) {
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
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		// ConsoleToken/ConsolePort left empty
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/get", token, agentGetFileRequest{Path: "/etc/x"})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAgentFileDeniesWhenGuestAgentConsoleNotEnabled(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.GuestAgent.Console left false
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/get", token, agentGetFileRequest{Path: "/etc/x"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when spec.guestAgent.console is false, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAgentFileDeniesWhenNotRunning(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Console: true}},
		Status:   model.MachineStatus{Phase: "Stopped"},
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/put", token, agentPutFileRequest{Path: "/etc/x", ContentBase64: "aGVsbG8="})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a non-Running machine, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleAgentFileFullRelay exercises the entire chain for both put and
// get: kairon-ui's handleAgentPutFile/handleAgentGetFile -> a real
// internal/consoleproxy.Server (standing in for kairon-node) -> a fake
// FluxVM server answering agent/put-file and agent/get-file.
// TestHandleAgentPutFileRejectsBodyOverTheAgentFileLimit proves the
// maxAgentFileBodyBytes override actually applies to a real HTTP request
// on this route, closing machine-guest-agent-files.md's own documented
// gap ("Writes have no such cap enforced on Kairon's side"). No relay/
// FluxVM server is needed -- decodeJSONWithLimit rejects the oversized
// body before handleAgentPutFile ever reaches the node relay.
func TestHandleAgentPutFileRejectsBodyOverTheAgentFileLimit(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Console: true}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	oversized := strings.Repeat("a", maxAgentFileBodyBytes+1)
	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/put", token, agentPutFileRequest{Path: "/etc/x", ContentBase64: oversized})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a body over maxAgentFileBodyBytes, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAgentFileFullRelay(t *testing.T) {
	var gotPutBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/runtime-1/agent/put-file":
			_ = json.NewDecoder(r.Body).Decode(&gotPutBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-written"})
		case "/v1/vms/runtime-1/agent/get-file":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "file-content", "content_base64": "aGVsbG8=", "mode": 420})
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

	putRR := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/put", token, agentPutFileRequest{Path: "/etc/app/config", ContentBase64: "aGVsbG8="})
	if putRR.Code != http.StatusOK {
		t.Fatalf("put: expected 200, got %d: %s", putRR.Code, putRR.Body.String())
	}
	if gotPutBody["path"] != "/etc/app/config" {
		t.Fatalf("expected the put request to reach FluxVM through both relay hops, got %+v", gotPutBody)
	}

	getRR := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/get", token, agentGetFileRequest{Path: "/etc/hostname"})
	if getRR.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	var out agentFileResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ContentBase64 != "aGVsbG8=" || out.Mode != 0o644 {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestHandleAgentFilePropagatesNodeRelayFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "guest agent is not enabled for this VM", http.StatusBadRequest)
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/agent-file/put", token, agentPutFileRequest{Path: "/etc/x", ContentBase64: "aGVsbG8="})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the request, got %d: %s", rr.Code, rr.Body.String())
	}
}
