// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleNetworkObservabilityFullRelay(t *testing.T) {
	var gotPaths []string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"items":[]}`))
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
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin -- read-only diagnostics are any-operator
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	for _, path := range []string{
		"/api/v1/machines/default/vm1/network-effective",
		"/api/v1/machines/default/vm1/network-stats",
		"/api/v1/machines/default/vm1/network-flows?limit=10",
		"/api/v1/machines/default/vm1/network-drop-reasons?limit=5",
	} {
		rr := doJSON(t, h, http.MethodGet, path, token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, rr.Code, rr.Body.String())
		}
	}
	expected := []string{
		"/v1/vms/runtime-1/network/effective",
		"/v1/vms/runtime-1/network/stats",
		"/v1/vms/runtime-1/network/flows?limit=10",
		"/v1/vms/runtime-1/network/drop-reasons?limit=5",
	}
	if len(gotPaths) != len(expected) {
		t.Fatalf("unexpected upstream calls: %v", gotPaths)
	}
	for i, p := range expected {
		if gotPaths[i] != p {
			t.Fatalf("call %d: expected %q, got %q", i, p, gotPaths[i])
		}
	}
}
