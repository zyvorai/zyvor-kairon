// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// wakeKube serves one Machine that becomes Running once patched to
// powerState Running.
type wakeKube struct {
	mu      sync.Mutex
	m       model.Machine
	patches []map[string]any
}

func (k *wakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(k.m)
	case http.MethodPatch:
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		k.patches = append(k.patches, p)
		if spec, _ := p["spec"].(map[string]any); spec["powerState"] == "Running" {
			k.m.Status.Phase = "Running"
			k.m.Status.RuntimeID, k.m.Status.NodeName = "rt", "n1"
		}
		_, _ = w.Write([]byte(`{}`))
	}
}

func wakeServer(t *testing.T, m model.Machine) (*Server, *wakeKube) {
	t.Helper()
	k := &wakeKube{m: m}
	srv := httptest.NewServer(k)
	t.Cleanup(srv.Close)
	activityTouches = sync.Map{}
	wakePollInterval = 10 * time.Millisecond
	return &Server{Kube: &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}}, k
}

func idleMachine(phase string, wake bool, suspended bool) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "sb", Namespace: "default"},
		Spec:     model.MachineSpec{Lifecycle: &model.LifecycleSpec{IdleTimeoutSeconds: 300, WakeOnConnect: wake, WakeTimeoutSeconds: 1}},
		Status:   model.MachineStatus{Phase: phase},
	}
	if suspended {
		m.Metadata.Annotations = map[string]string{model.AnnotationIdleSuspended: "2026-10-14T12:00:00Z"}
	}
	return m
}

func TestEnsureAwakeResumesIdleSuspended(t *testing.T) {
	s, k := wakeServer(t, idleMachine("Hibernated", true, true))
	got, woke, err := s.ensureAwake(context.Background(), k.m)
	if err != nil || !woke || got.Status.Phase != "Running" {
		t.Fatalf("got phase=%q woke=%v err=%v", got.Status.Phase, woke, err)
	}
	ann := k.patches[0]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if v, ok := ann[model.AnnotationIdleSuspended]; !ok || v != nil {
		t.Fatalf("idle-suspended not cleared: %v", ann)
	}
}

func TestEnsureAwakePausedIsWarm(t *testing.T) {
	s, k := wakeServer(t, idleMachine("Paused", true, true))
	if _, woke, err := s.ensureAwake(context.Background(), k.m); err != nil || woke {
		t.Fatalf("woke=%v err=%v", woke, err)
	}
}

func TestEnsureAwakeLeavesManualOrOptedOut(t *testing.T) {
	for name, m := range map[string]model.Machine{
		"manual pause":     idleMachine("Paused", true, false),
		"wake not enabled": idleMachine("Paused", false, true),
	} {
		s, k := wakeServer(t, m)
		got, _, err := s.ensureAwake(context.Background(), k.m)
		if err != nil || got.Status.Phase != "Paused" || len(k.patches) != 0 {
			t.Fatalf("%s: phase=%q patches=%v err=%v", name, got.Status.Phase, k.patches, err)
		}
	}
}

func TestEnsureAwakeTimesOut(t *testing.T) {
	s, k := wakeServer(t, idleMachine("Halted", true, true))
	k.m.Status.RuntimeID = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(k.m)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s.Kube = &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, _, err := s.ensureAwake(context.Background(), k.m); !errors.Is(err, errWakeTimeout) {
		t.Fatalf("err %v", err)
	}
}

func TestTouchActivityRateLimited(t *testing.T) {
	s, k := wakeServer(t, idleMachine("Running", false, false))
	k.m.Status.Phase = "Running"
	for i := 0; i < 3; i++ {
		if _, _, err := s.ensureAwake(context.Background(), k.m); err != nil {
			t.Fatal(err)
		}
	}
	if len(k.patches) != 1 {
		t.Fatalf("patches %d, want 1", len(k.patches))
	}
}
