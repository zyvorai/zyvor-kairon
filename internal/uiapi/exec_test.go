// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

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

func loginToken(t *testing.T, h http.Handler, username, password string) string {
	t.Helper()
	rr := login(t, h, username, password)
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if out.Token == "" {
		t.Fatalf("expected a non-empty session token, got body %s", rr.Body.String())
	}
	return out.Token
}

func newAdminServer(t *testing.T, fk *fakeKube) (*Server, string) {
	t.Helper()
	kubeSrv := httptest.NewServer(fk.handler())
	t.Cleanup(kubeSrv.Close)
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	return s, loginToken(t, h, "root", "correct-horse")
}

func TestHandleExecDeniesNonAdmin(t *testing.T) {
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleExecSandboxOperatorExec(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		label   string
		denied  bool
	}{
		{"off", false, "true", true},
		{"on, not a sandbox", true, "", true},
		{"on, sandbox", true, "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fk := newFakeKube()
			fk.machines["vm1"] = model.Machine{
				Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default", Labels: map[string]string{model.LabelSandbox: tc.label}},
				Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
				Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
			}
			kubeSrv := httptest.NewServer(fk.handler())
			defer kubeSrv.Close()
			s := &Server{
				Kube:                mustKubeClientAt(t, kubeSrv.URL),
				Users:               []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}},
				SessionSecret:       []byte("test-session-secret"),
				ConsoleToken:        "x",
				ConsolePort:         "8090",
				SandboxOperatorExec: tc.enabled,
			}
			h := s.Handler()
			token := loginToken(t, h, "alice", "correct-horse")
			rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
			if got := rr.Code == http.StatusForbidden; got != tc.denied {
				t.Fatalf("status %d (%s), want denied=%v", rr.Code, rr.Body.String(), tc.denied)
			}
		})
	}
}

func TestHandleExecDeniesWhenNotEnabled(t *testing.T) {
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
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		// ConsoleToken/ConsolePort left empty
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 when exec relay isn't configured, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleExecDeniesWhenNotRunning(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
		Status:   model.MachineStatus{Phase: "Stopped"},
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for a non-Running machine, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleExecDeniesWhenGuestAgentNotEnabled(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
		// Spec.GuestAgent.Enabled left false
	}
	s, token := newAdminServer(t, fk)
	h := s.Handler()

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when spec.guestAgent.enabled is false, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleExecAdminBypassesAllowlistButRBACStillApplies(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{
			Name: "vm1", Namespace: "default",
			Annotations: map[string]string{model.AnnotationConsoleAllowedUsers: "someone-else"},
		},
		Spec:   model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
		Status: model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.sar = func(model.SubjectAccessReview) model.SubjectAccessReviewStatus {
		return model.SubjectAccessReviewStatus{Allowed: false}
	}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
		RBACConsoleCheck: true,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403: an admin bypasses the annotation allowlist but a failed RBAC SubjectAccessReview must still deny, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestHandleExecFullRelay exercises the entire chain: kairon-ui's
// handleExec -> a real internal/consoleproxy.Server (standing in for
// kairon-node) -> a fake FluxVM server answering qga/exec.
func TestHandleExecFullRelay(t *testing.T) {
	var gotExecBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/runtime-1/qga/exec" {
			_ = json.NewDecoder(r.Body).Decode(&gotExecBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": "hello\n", "stderr": ""})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "runtime-1", "status": "Running"})
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo", Args: []string{"hello"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out execResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ExitCode != 0 || out.Stdout != "hello\n" {
		t.Fatalf("unexpected response: %+v", out)
	}
	if gotExecBody["path"] != "/bin/echo" {
		t.Fatalf("expected the request to reach FluxVM through both relay hops, got %+v", gotExecBody)
	}
}

func TestHandleExecPropagatesNodeRelayFailure(t *testing.T) {
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

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/exec", token, execRequest{Path: "/bin/echo"})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the node relay/FluxVM rejects the exec, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "guest agent is not enabled for this VM") {
		t.Fatalf("expected the relay's plain-text reason to reach the caller, got %s", rr.Body.String())
	}
}
