// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// TestHibernatePowerState pins the --hibernate mapping onto powerState:
// idempotent in both directions, false never starts a Stopped/Halted
// Machine, and true refuses when there is no runtime to save.
func TestHibernatePowerState(t *testing.T) {
	for _, tc := range []struct {
		current string
		want    bool
		state   string
		err     bool
	}{
		{"Running", true, "Hibernated", false},
		{"Paused", true, "Hibernated", false},
		{"Hibernated", true, "", false},
		{"Stopped", true, "", true},
		{"Halted", true, "", true},
		{"Hibernated", false, "Running", false},
		{"Running", false, "", false},
		{"Stopped", false, "", false},
		{"Halted", false, "", false},
	} {
		state, err := hibernatePowerState(tc.current, tc.want)
		if state != tc.state || (err != nil) != tc.err {
			t.Errorf("hibernatePowerState(%q, %t) = %q, %v; want %q, err=%t", tc.current, tc.want, state, err, tc.state, tc.err)
		}
	}
}

// hibernateEditServer serves one Machine on GET and records the PATCH body.
type hibernateEditServer struct {
	machine model.Machine
	patch   map[string]any
}

func (s *hibernateEditServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/db" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(s.machine)
		case http.MethodPatch:
			if err := json.NewDecoder(r.Body).Decode(&s.patch); err != nil {
				t.Errorf("decode patch: %v", err)
			}
			_ = json.NewEncoder(w).Encode(s.machine)
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}
}

// TestCmdEditMachineHibernate confirms --hibernate=true writes
// powerState Hibernated, --hibernate=false writes Running for a
// hibernated Machine, both pinned to the read resourceVersion, and that a
// no-op (already in the wanted state) sends no PATCH at all.
func TestCmdEditMachineHibernate(t *testing.T) {
	for _, tc := range []struct {
		flag, current, wantState string
	}{
		{"--hibernate=true", "Running", "Hibernated"},
		{"--hibernate", "Running", "Hibernated"},
		{"--hibernate=false", "Hibernated", "Running"},
		{"--hibernate=false", "Stopped", ""},
		{"--hibernate=true", "Hibernated", ""},
	} {
		s := &hibernateEditServer{machine: model.Machine{
			Metadata: model.ObjectMeta{Name: "db", Namespace: "default", ResourceVersion: "42"},
			Spec:     model.MachineSpec{PowerState: tc.current},
		}}
		srv := httptest.NewServer(s.handler(t))
		kc := &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
		out := captureStdout(t, func() { cmdEdit(context.Background(), kc, []string{"machine", "db", tc.flag}) })
		srv.Close()
		if tc.wantState == "" {
			if s.patch != nil || !strings.Contains(out, "unchanged") {
				t.Errorf("%s on %s: want no PATCH and 'unchanged', got patch=%v out=%q", tc.flag, tc.current, s.patch, out)
			}
			continue
		}
		spec, _ := s.patch["spec"].(map[string]any)
		meta, _ := s.patch["metadata"].(map[string]any)
		if spec["powerState"] != tc.wantState || len(spec) != 1 || meta["resourceVersion"] != "42" {
			t.Errorf("%s on %s: patch = %v, want only powerState=%s at resourceVersion 42", tc.flag, tc.current, s.patch, tc.wantState)
		}
	}
}

// TestCmdEditMachinePriorityUnchanged confirms --priority alone still
// sends a plain spec.priority patch without reading the Machine first.
func TestCmdEditMachinePriorityUnchanged(t *testing.T) {
	s := &recordingServer{}
	kc := testClient(t, s)
	cmdEdit(context.Background(), kc, []string{"machine", "db", "--priority", "7"})
	if s.method != http.MethodPatch {
		t.Fatalf("method=%s, want PATCH", s.method)
	}
	spec, _ := s.body["spec"].(map[string]any)
	if len(spec) != 1 || spec["priority"] != float64(7) || s.body["metadata"] != nil {
		t.Errorf("patch = %v, want only spec.priority=7", s.body)
	}
}

// TestCmdGetMachinesShowsHibernated confirms the HIBERNATED column reports
// the Hibernated condition status and "-" when there is none, matching the
// CRD's Hibernated printer column.
func TestCmdGetMachinesShowsHibernated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{
			{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Hibernated", Conditions: []model.Condition{{Type: "Ready", Status: "False"}, {Type: "Hibernated", Status: "True", Reason: "StateSaved"}}}},
			{Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Running", Conditions: []model.Condition{{Type: "Hibernated", Status: "False", Reason: "Resumed"}}}},
			{Metadata: model.ObjectMeta{Name: "new", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Running"}},
		}})
	}))
	t.Cleanup(srv.Close)
	kc := &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	out := captureStdout(t, func() { cmdGet(context.Background(), kc, []string{"machines", "-n", "prod"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasSuffix(lines[0], "\tHIBERNATED") || !strings.HasSuffix(lines[1], "\tTrue") || !strings.HasSuffix(lines[2], "\tFalse") || !strings.HasSuffix(lines[3], "\t-") {
		t.Fatalf("unexpected get machines output:\n%s", out)
	}
}
