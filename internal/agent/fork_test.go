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
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func forkTestAgent(t *testing.T, parentNode string, existingChild bool) (*Agent, *[]map[string]any) {
	t.Helper()
	parent := model.Machine{
		Metadata: model.ObjectMeta{Name: "base", Namespace: "agents"},
		Spec:     model.MachineSpec{NodeName: parentNode},
		Status:   model.MachineStatus{RuntimeID: "parent-id"},
	}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/agents/machines/base" {
			_ = json.NewEncoder(w).Encode(parent)
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(ks.Close)
	var forks []map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == "kairon-agents-kid-1":
			if existingChild {
				_ = json.NewEncoder(w).Encode([]fluxvm.Record{{IDValue: "kid-id", Name: "kairon-agents-kid-1", Status: "Running"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/parent-id":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{IDValue: "parent-id", Status: "Running"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/parent-id/fork":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			forks = append(forks, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"items":[{"id":"kid-id","name":"kairon-agents-kid-1","status":"Running"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
		}
	}))
	t.Cleanup(fs.Close)
	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	return &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &forks
}

func forkChild() model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "kid", Namespace: "agents", Annotations: map[string]string{model.AnnotationForkFrom: "base"}},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
	}
}

func TestForkRuntimeForksParent(t *testing.T) {
	a, forks := forkTestAgent(t, "worker-1", false)
	rec, err := a.forkRuntime(context.Background(), forkChild())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID() != "kid-id" || len(*forks) != 1 || (*forks)[0]["namePrefix"] != "kairon-agents-kid" || (*forks)[0]["count"] != float64(1) {
		t.Fatalf("rec=%+v forks=%v", rec, *forks)
	}
}

func TestForkRuntimeAdoptsExistingChild(t *testing.T) {
	a, forks := forkTestAgent(t, "worker-1", true)
	rec, err := a.forkRuntime(context.Background(), forkChild())
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID() != "kid-id" || len(*forks) != 0 {
		t.Fatalf("an existing child must be adopted, not forked again: rec=%+v forks=%v", rec, *forks)
	}
}

func TestForkRuntimeRequiresParentOnSameNode(t *testing.T) {
	a, forks := forkTestAgent(t, "worker-2", false)
	_, err := a.forkRuntime(context.Background(), forkChild())
	if err == nil || !strings.Contains(err.Error(), "parent's node") || len(*forks) != 0 {
		t.Fatalf("err=%v forks=%v", err, *forks)
	}
}

func TestForkRuntimeRejectsVolumes(t *testing.T) {
	a, _ := forkTestAgent(t, "worker-1", false)
	kid := forkChild()
	kid.Spec.Volumes = []model.MachineVolume{{}}
	if _, err := a.forkRuntime(context.Background(), kid); err == nil || !strings.Contains(err.Error(), "copy-on-write") {
		t.Fatalf("err=%v", err)
	}
}
