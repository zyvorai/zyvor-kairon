// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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

// nodeRelaySetup stands up a real internal/consoleproxy.Server (kairon-node)
// backed by a fake FluxVM server, and returns the node's host/port for a
// uiapi Server's ConsolePort/ConsoleToken to reach it -- the same
// full-relay shape TestHandleVMRestoreSnapshotFullRelay etc. already use.
func nodeRelaySetup(t *testing.T, fluxHandler http.HandlerFunc) (nodeHost, nodePort, relayToken string, cleanup func()) {
	t.Helper()
	fluxSrv := httptest.NewServer(fluxHandler)
	relayToken = "shared-node-relay-token"
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: relayToken}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	nodeURL, err := url.Parse(nodeSrv.URL)
	if err != nil {
		t.Fatalf("parse node server URL: %v", err)
	}
	nodeHost, nodePort, err = net.SplitHostPort(nodeURL.Host)
	if err != nil {
		t.Fatalf("split node server host/port: %v", err)
	}
	return nodeHost, nodePort, relayToken, func() {
		fluxSrv.Close()
		nodeSrv.Close()
	}
}

func TestHandleListNodeSandboxesFullRelay(t *testing.T) {
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []fluxvm.Record{{UUID: "sandbox-1", Status: "Running"}}})
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
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/worker-1/sandboxes", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleBuildTemplateDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "worker-1"}}}
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/templates", token, buildTemplateRequest{Name: "alpine-agent", ImageRef: "docker.io/library/alpine:3.19"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleBuildTemplateFullRelay(t *testing.T) {
	var gotBody map[string]any
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.TemplateInfo{Name: "alpine-agent"})
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
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/templates", token, buildTemplateRequest{Name: "alpine-agent", ImageRef: "docker.io/library/alpine:3.19"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotBody["name"] != "alpine-agent" || gotBody["image_ref"] != "docker.io/library/alpine:3.19" {
		t.Fatalf("expected the request to reach FluxVM through both relay hops, got %+v", gotBody)
	}
}

func TestHandleSandboxHTTPProxyDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.machines["sb1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "sb1", Namespace: "default"},
		Spec:     model.MachineSpec{Sandbox: &model.SandboxSpec{}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "sandbox-1"},
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/default/sb1/sandbox-http/8080/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSandboxHTTPProxyRequiresSandboxSpec(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.Sandbox left nil
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/default/vm1/sandbox-http/8080/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when spec.sandbox is unset, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSandboxHTTPProxyFullRelay(t *testing.T) {
	var gotPath, gotMethod string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("X-Guest-Header", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("guest response"))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.machines["sb1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "sb1", Namespace: "default"},
		Spec:     model.MachineSpec{Sandbox: &model.SandboxSpec{}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "sandbox-1"},
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/machines/default/sb1/sandbox-http/8080/api/webhook", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Guest-Header") != "yes" || rr.Body.String() != "guest response" {
		t.Fatalf("expected the guest's response to be relayed back, got headers=%+v body=%q", rr.Header(), rr.Body.String())
	}
	if gotPath != "/v1/sandboxes/sandbox-1/http/8080/api/webhook" || gotMethod != http.MethodPost {
		t.Fatalf("unexpected upstream request: path=%q method=%q", gotPath, gotMethod)
	}
}
