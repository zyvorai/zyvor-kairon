// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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

func TestHandleQGAFsfreezeStatusDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
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

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/qga/fsfreeze-status", token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleQGADeniesWhenGuestAgentNotEnabled(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.GuestAgent.Enabled left false
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/qga/fsfreeze-status", token, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when spec.guestAgent.enabled is false, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleQGAFullRelay exercises the entire chain for fsfreeze-status
// and both firewall routes: kairon-ui -> a real
// internal/consoleproxy.Server (standing in for kairon-node) -> a fake
// FluxVM server.
func TestHandleQGAFullRelay(t *testing.T) {
	var gotFirewallOpenBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/runtime-1/qga/fsfreeze-status":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "thawed"})
		case "/v1/vms/runtime-1/qga/firewall/open":
			_ = json.NewDecoder(r.Body).Decode(&gotFirewallOpenBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "", "stderr": ""})
		case "/v1/vms/runtime-1/qga/firewall/close":
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "", "stderr": ""})
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
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
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

	statusRR := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/qga/fsfreeze-status", token, nil)
	if statusRR.Code != http.StatusOK {
		t.Fatalf("fsfreeze-status: expected 200, got %d: %s", statusRR.Code, statusRR.Body.String())
	}
	var statusOut fsfreezeStatusResponse
	if err := json.Unmarshal(statusRR.Body.Bytes(), &statusOut); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if statusOut.Status != "thawed" {
		t.Fatalf("unexpected fsfreeze-status response: %+v", statusOut)
	}

	openRR := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/qga/firewall/open", token, firewallOpenRequest{Name: "web", Port: 8080, Protocol: "tcp"})
	if openRR.Code != http.StatusOK {
		t.Fatalf("firewall/open: expected 200, got %d: %s", openRR.Code, openRR.Body.String())
	}
	if gotFirewallOpenBody["name"] != "web" || gotFirewallOpenBody["port"] != float64(8080) {
		t.Fatalf("expected the open request to reach FluxVM through both relay hops, got %+v", gotFirewallOpenBody)
	}

	closeRR := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/qga/firewall/close", token, firewallCloseRequest{Name: "web"})
	if closeRR.Code != http.StatusOK {
		t.Fatalf("firewall/close: expected 200, got %d: %s", closeRR.Code, closeRR.Body.String())
	}
}

func TestHandleQGAPropagatesNodeRelayFailure(t *testing.T) {
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
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
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

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/qga/fsfreeze-status", token, nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the request, got %d: %s", rr.Code, rr.Body.String())
	}
}
