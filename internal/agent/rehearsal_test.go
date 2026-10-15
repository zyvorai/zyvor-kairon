// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/rehearsal"
)

func TestRehearsalIsolationRefusesBootAndStopsExistingCopy(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "refuse-boot", true: "stop-existing"}[running], func(t *testing.T) {
			ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/nodes/lab" {
					t.Errorf("unexpected kube request %s %s", r.Method, r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(model.Node{}) // No valid reservation.
			}))
			defer ks.Close()
			stopped := false
			fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/v1/vms":
					_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
				case r.Method == "GET" && r.URL.Path == "/v1/vms/copy":
					_ = json.NewEncoder(w).Encode(fluxvm.Record{IDValue: "copy", Status: "Running"})
				case r.Method == "POST" && r.URL.Path == "/v1/vms/copy/stop":
					stopped = true
					_ = json.NewEncoder(w).Encode(fluxvm.Record{IDValue: "copy", Status: "Stopped"})
				default:
					t.Errorf("unsafe/unexpected FluxVM call %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusBadRequest)
				}
			}))
			defer fs.Close()
			kc := &kube.Client{BaseURL: ks.URL, HTTP: ks.Client()}
			fc := fluxvm.New(fs.URL, "")
			fc.HTTP = fs.Client()
			a := &Agent{NodeName: "lab", Kube: kc, Flux: fc}
			m := model.Machine{Metadata: model.ObjectMeta{Name: "copy", Namespace: "default", Finalizers: []string{model.Finalizer}, Labels: map[string]string{rehearsal.RunLabel: "run"}, Annotations: map[string]string{rehearsal.GuardAnnotation: "v1"}}, Spec: model.MachineSpec{PowerState: "Running", Network: model.NetworkSpec{Mode: "tap", Bridge: "krh-no-such"}}}
			if running {
				m.Status.RuntimeID = "copy"
			}
			if err := a.reconcileMachine(context.Background(), m); err == nil {
				t.Fatal("accepted invalid isolation")
			}
			if stopped != running {
				t.Fatalf("running=%t stop requested=%t", running, stopped)
			}
		})
	}
}
