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

func TestHandleRestartMachineSetsTheRequestAnnotation(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{PowerState: "Running"},
		Status:   model.MachineStatus{Phase: "Running"},
	}
	s, token := newAdminServer(t, fk)
	rr := doJSON(t, s.Handler(), http.MethodPost, "/api/v1/machines/default/vm1/restart", token, map[string]any{})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	patches := fk.patches["/apis/kairon.zyvor.dev/v1/namespaces/default/machines/vm1"]
	if len(patches) != 1 || !strings.Contains(patches[0], model.AnnotationRestartRequested) {
		t.Fatalf("patches %v", patches)
	}
}

func TestHandleRestartMachineRefusesAStoppedMachine(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{PowerState: "Stopped"},
	}
	s, token := newAdminServer(t, fk)
	if rr := doJSON(t, s.Handler(), http.MethodPost, "/api/v1/machines/default/vm1/restart", token, map[string]any{}); rr.Code != http.StatusConflict {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSoftRebootFallsBackToPowerShell(t *testing.T) {
	var bodies []map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/runtime-1/qga/exec" {
			http.NotFound(w, r)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		code := 127
		if b["powershell"] != nil {
			code = 0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": code})
	}))
	defer fluxSrv.Close()
	nodeRelay := &consoleproxy.Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "relay"}
	nodeSrv := httptest.NewServer(nodeRelay.Handler())
	defer nodeSrv.Close()
	u, _ := url.Parse(nodeSrv.URL)
	host, port, _ := net.SplitHostPort(u.Host)

	fk := newFakeKube()
	fk.machines["vm1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default"},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "runtime-1"},
	}
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: host}}},
	}}
	s, token := newAdminServer(t, fk)
	s.ConsoleToken, s.ConsolePort = "relay", port
	rr := doJSON(t, s.Handler(), http.MethodPost, "/api/v1/machines/default/vm1/soft-reboot", token, map[string]any{})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if len(bodies) != 2 || bodies[0]["path"] != "/bin/sh" || bodies[1]["powershell"] != "Restart-Computer -Force" {
		t.Fatalf("exec calls %v", bodies)
	}
}
