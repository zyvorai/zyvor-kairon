// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func lifecycleMachine(l model.LifecycleSpec, created, activity time.Time) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "sb", Namespace: "ai", UID: "u", CreationTimestamp: created},
		Spec:     model.MachineSpec{Lifecycle: &l},
		Status:   model.MachineStatus{Phase: "Running"},
	}
	if !activity.IsZero() {
		m.Status.Lifecycle = &model.LifecycleStatus{LastActivityTime: activity.UTC().Format(time.RFC3339)}
	}
	return m
}

func TestLifecycleDecision(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	created := now.Add(-time.Hour)
	cases := []struct { //nolint:prealloc // table literal, not a loop-built slice
		name   string
		m      model.Machine
		action string
	}{
		{"expired", lifecycleMachine(model.LifecycleSpec{MaxLifetimeSeconds: 3600}, created, now), lifecycleExpire},
		{"not expired", lifecycleMachine(model.LifecycleSpec{MaxLifetimeSeconds: 7200}, created, now), ""},
		{"idle pause default", lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, now.Add(-10*time.Minute)), model.IdleActionPause},
		{"idle delete", lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300, IdleAction: model.IdleActionDelete}, created, now.Add(-10*time.Minute)), model.IdleActionDelete},
		{"recent activity", lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, now.Add(-time.Minute)), ""},
		{"no activity reported yet", lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, time.Time{}), ""},
	}
	paused := lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, now.Add(-time.Hour))
	paused.Status.Phase = "Paused"
	cases = append(cases, struct {
		name   string
		m      model.Machine
		action string
	}{"already paused", paused, ""})
	migrating := lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, now.Add(-time.Hour))
	migrating.Metadata.Annotations = map[string]string{model.AnnotationMigrationRef: "mig-1"}
	cases = append(cases, struct {
		name   string
		m      model.Machine
		action string
	}{"migrating", migrating, ""})
	relay := lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300}, created, now.Add(-time.Hour))
	relay.Metadata.Annotations = map[string]string{model.AnnotationLastActivity: now.Add(-time.Minute).Format(time.RFC3339)}
	cases = append(cases, struct {
		name   string
		m      model.Machine
		action string
	}{"relay activity counts", relay, ""})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, _ := lifecycleDecision(tc.m, now); got != tc.action {
				t.Fatalf("action %q, want %q", got, tc.action)
			}
		})
	}
}

func TestReconcileLifecyclePatchesPowerState(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var patch map[string]any
	var events, deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/machines/idle"):
			_ = json.NewDecoder(r.Body).Decode(&patch)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/machines/old"):
			deletes++
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			events++
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := &Controller{Kube: &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	idle := lifecycleMachine(model.LifecycleSpec{IdleTimeoutSeconds: 300, IdleAction: model.IdleActionHibernate}, now.Add(-time.Hour), now.Add(-time.Hour))
	idle.Metadata.Name = "idle"
	old := lifecycleMachine(model.LifecycleSpec{MaxLifetimeSeconds: 60}, now.Add(-time.Hour), now)
	old.Metadata.Name = "old"
	c.reconcileLifecycle(context.Background(), []model.Machine{idle, old}, now)

	spec, _ := patch["spec"].(map[string]any)
	if spec["powerState"] != "Hibernated" {
		t.Fatalf("patch %v", patch)
	}
	ann, _ := patch["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[model.AnnotationIdleSuspended] != now.Format(time.RFC3339) {
		t.Fatalf("annotations %v", ann)
	}
	if deletes != 1 || events != 2 {
		t.Fatalf("deletes=%d events=%d", deletes, events)
	}
}

func TestLifecyclePoolMembers(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	warm := lifecycleMachine(model.LifecycleSpec{MaxLifetimeSeconds: 60, IdleTimeoutSeconds: 60}, now.Add(-time.Hour), now.Add(-time.Hour))
	warm.Metadata.Labels = map[string]string{model.LabelPoolState: model.PoolStateWarm}
	if got, _, _ := lifecycleDecision(warm, now); got != "" {
		t.Fatalf("warm member: %q", got)
	}
	claimed := lifecycleMachine(model.LifecycleSpec{MaxLifetimeSeconds: 600}, now.Add(-time.Hour), now)
	claimed.Metadata.Labels = map[string]string{model.LabelPoolState: model.PoolStateClaimed}
	claimed.Metadata.Annotations = map[string]string{model.AnnotationLifecycleStart: now.Add(-time.Minute).Format(time.RFC3339)}
	if got, _, _ := lifecycleDecision(claimed, now); got != "" {
		t.Fatalf("lifetime should count from bind: %q", got)
	}
}
