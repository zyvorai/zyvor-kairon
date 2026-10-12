// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func incidentServer(t *testing.T, flux http.HandlerFunc, users []User) (http.Handler, func()) {
	t.Helper()
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, flux)
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
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         users,
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	return s.Handler(), func() { kubeSrv.Close(); cleanup() }
}

func TestIncidentBundleDeniesNonAdmin(t *testing.T) {
	called := false
	h, done := incidentServer(t, func(w http.ResponseWriter, r *http.Request) { called = true },
		[]User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}})
	defer done()
	token := loginToken(t, h, "alice", "correct-horse")
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/machines/default/vm1/diagnostics"},
		{http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/tok"},
		{http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/tok/memory-dump"},
	} {
		rr := doJSON(t, h, c.method, c.path, token, map[string]any{})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403, got %d: %s", c.method, c.path, rr.Code, rr.Body.String())
		}
	}
	if called {
		t.Fatal("FluxVM was called for a non-admin")
	}
}

func TestIncidentBundleFullRelay(t *testing.T) {
	var gotBody map[string]any
	var calls []string
	h, done := incidentServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/vms/runtime-1/diagnostics":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"token":"tok","state":"running"}`))
		case "GET /v1/vms/runtime-1/diagnostics/tok":
			_, _ = w.Write([]byte(`{"token":"tok","state":"done","sha256":"ab"}`))
		case "GET /v1/vms/runtime-1/diagnostics/tok/bundle":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write([]byte("gz-bytes"))
		case "GET /v1/vms/runtime-1/diagnostics/busy/bundle":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"diagnostics bundle busy is running"}`))
		default:
			http.NotFound(w, r)
		}
	}, []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}})
	defer done()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/machines/default/vm1/diagnostics", token,
		map[string]any{"memory_dump": true, "redact": true, "pcap_seconds": 5})
	if rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"tok"`) {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	if gotBody["memory_dump"] != true || gotBody["redact"] != true || gotBody["pcap_seconds"] != float64(5) {
		t.Fatalf("FluxVM got %v", gotBody)
	}
	rr = doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/tok", token, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"done"`) {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/tok/bundle", token, nil)
	body, _ := io.ReadAll(rr.Body)
	if rr.Code != http.StatusOK || string(body) != "gz-bytes" || rr.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("bundle: %d %q %v", rr.Code, body, rr.Header())
	}
	rr = doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/busy/bundle", token, nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "is running") {
		t.Fatalf("busy: %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm1/diagnostics/tok/etc-passwd", token, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown file: %d", rr.Code)
	}
	if len(calls) != 4 {
		t.Fatalf("FluxVM calls %v", calls)
	}
}
