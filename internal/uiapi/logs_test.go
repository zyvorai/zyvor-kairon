// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zyvorai/kairon/internal/consoleproxy"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleLogsDeniesUserNotInAllowlist(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{
			Name: "vm1", Namespace: "default",
			Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "bob"},
		},
		Status: model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/logs", token, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a user not on the console allowlist, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLogsDisabledWhenNotConfigured(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL)} // ConsoleToken/ConsolePort left empty
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/logs", "", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleLogsRequiresRuntime(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Pending"}, // no RuntimeID yet
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), ConsoleToken: "x", ConsolePort: "8090"}
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/logs", "", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a machine with no runtime yet, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleLogsAllowsPausedMachine confirms logs don't require
// Phase == "Running" the way exec/console do -- a Paused Machine's log
// file still exists on the node.
func TestHandleLogsFullRelayAllowsPausedMachine(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/runtime-1/logs" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("boot line 1\nboot line 2\n"))
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

	// Metrics configured deliberately -- proves statusRecorder's own
	// Flush passthrough (server.go) doesn't break this response; without
	// it the response would still eventually arrive in a single test
	// (httptest.NewRecorder doesn't actually exercise TCP buffering the
	// way a real connection would), so what this really guards is that
	// wrapping the response through withMetrics doesn't panic or corrupt
	// the body.
	s := &Server{
		Kube: mustKubeClientAt(t, kubeSrv.URL), ConsoleToken: relayToken, ConsolePort: nodePort,
		Metrics: metrics.NewRecorder(),
	}
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/logs", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "boot line 1\nboot line 2\n" {
		t.Fatalf("unexpected body: %q", rr.Body.String())
	}
}

func TestHandleLogsPropagatesNodeRelayFailure(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM not found", http.StatusNotFound)
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

	s := &Server{Kube: mustKubeClientAt(t, kubeSrv.URL), ConsoleToken: relayToken, ConsolePort: nodePort}
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/logs", "", nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the request, got %d: %s", rr.Code, rr.Body.String())
	}
}
