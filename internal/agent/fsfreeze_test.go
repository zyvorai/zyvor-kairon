// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func newFreezeTestAgent(t *testing.T, guestState string) (*Agent, *map[string]any, *[]string) {
	t.Helper()
	var patched map[string]any
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1":
			var p struct {
				Metadata struct {
					Annotations map[string]any `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			patched = p.Metadata.Annotations
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/prod/events":
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	var calls []string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-123/qga/fsfreeze":
			calls = append(calls, "freeze")
			_ = json.NewEncoder(w).Encode(map[string]int64{"frozen": 1})
		case "/v1/vms/vm-123/qga/fsthaw":
			calls = append(calls, "thaw")
			_ = json.NewEncoder(w).Encode(map[string]int64{"thawed": 1})
		case "/v1/vms/vm-123/qga/fsfreeze-status":
			calls = append(calls, "status")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": guestState})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(fs.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	return &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &patched, &calls
}

func TestReconcileGuestFreeze(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(time.Minute).Format(time.RFC3339), now.Add(-time.Second).Format(time.RFC3339)
	cases := []struct {
		name       string
		guest      string
		annos      map[string]string
		wantCalls  string
		wantFrozen any
	}{
		{"freeze on request", "thawed", map[string]string{model.AnnotationFsFreezeUntil: future}, "freeze", now.Format(time.RFC3339)},
		{"already frozen", "frozen", map[string]string{model.AnnotationFsFreezeUntil: future, model.AnnotationFsFrozen: "x"}, "status", "unpatched"},
		{"refreeze after outside thaw", "thawed", map[string]string{model.AnnotationFsFreezeUntil: future, model.AnnotationFsFrozen: "x"}, "status,freeze", "unpatched"},
		{"auto-thaw at deadline", "frozen", map[string]string{model.AnnotationFsFreezeUntil: past, model.AnnotationFsFrozen: "x"}, "thaw", nil},
		{"unfreeze", "frozen", map[string]string{model.AnnotationFsFrozen: "x"}, "thaw", nil},
		{"quiesce owns it", "thawed", map[string]string{model.AnnotationFsFreezeUntil: future, model.AnnotationQuiesceStatus: "snap"}, "", "unpatched"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, patched, calls := newFreezeTestAgent(t, tc.guest)
			m := model.Machine{
				Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod", Annotations: tc.annos},
				Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
			}
			a.reconcileGuestFreeze(context.Background(), m, &fluxvm.Record{UUID: "vm-123"}, now)
			got := ""
			for i, c := range *calls {
				if i > 0 {
					got += ","
				}
				got += c
			}
			if got != tc.wantCalls {
				t.Fatalf("calls %q, want %q", got, tc.wantCalls)
			}
			if tc.wantFrozen == "unpatched" {
				if *patched != nil {
					t.Fatalf("unexpected patch %v", *patched)
				}
				return
			}
			if v, ok := (*patched)[model.AnnotationFsFrozen]; !ok || v != tc.wantFrozen {
				t.Fatalf("fs-frozen patch %v, want %v", *patched, tc.wantFrozen)
			}
		})
	}
}
