// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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

func newQuiesceTestAgent(t *testing.T) (*Agent, *map[string]any, *[]string) {
	t.Helper()
	var patchedAnnos map[string]any
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1" {
			var patch struct {
				Metadata struct {
					Annotations map[string]any `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			patchedAnnos = patch.Metadata.Annotations
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(ks.Close)

	var fluxCalls []string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-123/qga/fsfreeze":
			fluxCalls = append(fluxCalls, "freeze")
			_ = json.NewEncoder(w).Encode(map[string]int64{"frozen": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-123/qga/fsthaw":
			fluxCalls = append(fluxCalls, "thaw")
			_ = json.NewEncoder(w).Encode(map[string]int64{"thawed": 1})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(fs.Close)

	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return a, &patchedAnnos, &fluxCalls
}

func quiesceTestMachine(annos map[string]string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod", Annotations: annos},
		Spec:     model.MachineSpec{GuestAgent: model.GuestAgentSpec{Enabled: true}},
	}
}

func TestReconcileGuestQuiesceFreezesOnRequest(t *testing.T) {
	a, patched, calls := newQuiesceTestAgent(t)
	ref := model.FormatQuiesceRef("snap", time.Now())
	m := quiesceTestMachine(map[string]string{model.AnnotationQuiesceRequest: ref})
	a.reconcileGuestQuiesce(context.Background(), m, &fluxvm.Record{UUID: "vm-123"})
	if len(*calls) != 1 || (*calls)[0] != "freeze" {
		t.Fatalf("expected exactly one freeze call, got %v", *calls)
	}
	if (*patched)[model.AnnotationQuiesceStatus] != ref {
		t.Fatalf("expected the status annotation to be set to %q, got %+v", ref, *patched)
	}
}

func TestReconcileGuestQuiesceThawsWhenRequestCleared(t *testing.T) {
	a, patched, calls := newQuiesceTestAgent(t)
	ref := model.FormatQuiesceRef("snap", time.Now())
	m := quiesceTestMachine(map[string]string{model.AnnotationQuiesceStatus: ref}) // frozen, no request -- thaw
	a.reconcileGuestQuiesce(context.Background(), m, &fluxvm.Record{UUID: "vm-123"})
	if len(*calls) != 1 || (*calls)[0] != "thaw" {
		t.Fatalf("expected exactly one thaw call, got %v", *calls)
	}
	if v, ok := (*patched)[model.AnnotationQuiesceStatus]; !ok || v != nil {
		t.Fatalf("expected the status annotation to be cleared (null), got %+v", *patched)
	}
}

func TestReconcileGuestQuiesceDoesNothingWhenAlreadyConfirmed(t *testing.T) {
	a, patched, calls := newQuiesceTestAgent(t)
	ref := model.FormatQuiesceRef("snap", time.Now())
	m := quiesceTestMachine(map[string]string{model.AnnotationQuiesceRequest: ref, model.AnnotationQuiesceStatus: ref})
	a.reconcileGuestQuiesce(context.Background(), m, &fluxvm.Record{UUID: "vm-123"})
	if len(*calls) != 0 {
		t.Fatalf("expected no FluxVM calls once already confirmed frozen, got %v", *calls)
	}
	if *patched != nil {
		t.Fatalf("expected no Machine patch, got %+v", *patched)
	}
}

func TestReconcileGuestQuiesceDoesNothingWhenGuestAgentDisabled(t *testing.T) {
	a, patched, calls := newQuiesceTestAgent(t)
	ref := model.FormatQuiesceRef("snap", time.Now())
	m := quiesceTestMachine(map[string]string{model.AnnotationQuiesceRequest: ref})
	m.Spec.GuestAgent.Enabled = false
	a.reconcileGuestQuiesce(context.Background(), m, &fluxvm.Record{UUID: "vm-123"})
	if len(*calls) != 0 {
		t.Fatalf("expected no FluxVM calls without guestAgent.enabled, got %v", *calls)
	}
	if *patched != nil {
		t.Fatalf("expected no Machine patch, got %+v", *patched)
	}
}

func TestReconcileGuestQuiesceDoesNothingWithNoAnnotations(t *testing.T) {
	a, patched, calls := newQuiesceTestAgent(t)
	m := quiesceTestMachine(nil)
	a.reconcileGuestQuiesce(context.Background(), m, &fluxvm.Record{UUID: "vm-123"})
	if len(*calls) != 0 {
		t.Fatalf("expected no FluxVM calls with no quiesce annotations at all, got %v", *calls)
	}
	if *patched != nil {
		t.Fatalf("expected no Machine patch, got %+v", *patched)
	}
}
