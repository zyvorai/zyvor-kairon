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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type sessionWorld struct {
	mu         sync.Mutex
	sessions   map[string]*model.AgentSession
	nodeLabels map[string]any
	flux       map[string]map[string]any
	creates    int
	deletes    []string
	createBody map[string]any
	block      chan struct{}
}

func mergeInto(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, pm)
				continue
			}
		}
		dst[k] = v
	}
}

func newSessionWorld(t *testing.T, sessions ...model.AgentSession) (*Agent, *sessionWorld) {
	t.Helper()
	prevOS := agentSessionHostOS
	agentSessionHostOS = "darwin"
	t.Cleanup(func() { agentSessionHostOS = prevOS })
	w := &sessionWorld{sessions: map[string]*model.AgentSession{}, nodeLabels: map[string]any{}, flux: map[string]map[string]any{}}
	for i := range sessions {
		s := sessions[i]
		w.sessions[s.Metadata.Name] = &s
	}
	const prefix = "/apis/kairon.zyvor.dev/v1/namespaces/prod/agentsessions/"
	ks := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/agentsessions":
			if !strings.Contains(r.URL.Query().Get("labelSelector"), "mac-1") {
				t.Errorf("selector = %q", r.URL.Query().Get("labelSelector"))
			}
			list := model.AgentSessionList{}
			for _, s := range w.sessions {
				list.Items = append(list.Items, *s)
			}
			_ = json.NewEncoder(rw).Encode(list)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, prefix):
			name, isStatus := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/status")
			s := w.sessions[name]
			cur := map[string]any{}
			raw, _ := json.Marshal(s)
			_ = json.Unmarshal(raw, &cur)
			if isStatus {
				body = map[string]any{"status": body["status"]}
			}
			mergeInto(cur, body)
			raw, _ = json.Marshal(cur)
			var next model.AgentSession
			_ = json.Unmarshal(raw, &next)
			w.sessions[name] = &next
			_, _ = io.WriteString(rw, `{}`)
		case r.URL.Path == "/api/v1/nodes/mac-1":
			if r.Method == http.MethodPatch {
				labels := body["metadata"].(map[string]any)["labels"].(map[string]any)
				mergeInto(w.nodeLabels, labels)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"metadata": map[string]any{"name": "mac-1", "labels": w.nodeLabels}})
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ks.Close)
	fs := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/agents/sessions" {
			w.mu.Lock()
			block := w.block
			w.mu.Unlock()
			if block != nil {
				<-block
			}
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/v1/agents/sessions/")
		switch {
		case r.URL.Path == "/v1/agents/sessions" && r.Method == http.MethodGet:
			items := []map[string]any{}
			for _, s := range w.flux {
				items = append(items, s)
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"items": items})
		case r.URL.Path == "/v1/agents/sessions" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&w.createBody)
			w.creates++
			s := fluxSession("11111111-0000-0000-0000-000000000001", "running", time.Now())
			w.flux[s["id"].(string)] = s
			rw.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(rw).Encode(s)
		case r.Method == http.MethodGet:
			s, ok := w.flux[id]
			if !ok {
				rw.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(rw, `{"error":"agent session not found"}`)
				return
			}
			_ = json.NewEncoder(rw).Encode(s)
		case r.Method == http.MethodDelete:
			w.deletes = append(w.deletes, id)
			delete(w.flux, id)
			rw.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(fs.Close)
	kc, err := kube.New(ks.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	return &Agent{NodeName: "mac-1", Kube: kc, Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, w
}

func fluxSession(id, state string, created time.Time) map[string]any {
	return map[string]any{
		"id": id, "agent": "custom", "sandbox_id": "sb-" + id[:4], "backend": "vz", "parent": nil, "children": []string{},
		"state": state, "created_at": created.UTC().Format(time.RFC3339Nano), "expires_at": nil,
		"capabilities": map[string]any{"snapshot": true, "fork": true, "pty": true, "confinement": false, "http_policy": false},
		"policy":       map[string]any{"preset": "offline", "offline": true, "allow_hosts": []string{}, "unenforced": []string{}},
		"workspace":    map[string]any{"path": "/workspace", "git": "https://x-token:s3cret@github.com/a/b", "mode": "clone", "cloned": true, "baseline": true, "note": nil},
		"checkpoints":  []any{map[string]any{"tag": "c1", "memory": true, "created_at": created.UTC().Format(time.RFC3339)}},
		"usage":        map[string]any{"execs": 3, "exec_ms": 1200, "jobs": 1, "children": 0},
	}
}

func scheduledSession(name string) model.AgentSession {
	return model.AgentSession{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Labels: map[string]string{model.AssignedNodeLabel: "mac-1"}},
		Spec: model.AgentSessionSpec{
			NodeName:  "mac-1",
			Workspace: &model.AgentWorkspace{Git: "https://github.com/a/b"},
			Resources: model.AgentSessionResources{CPU: "2", Memory: "4Gi"},
			Policy:    &model.AgentPolicy{Preset: "offline", Process: &model.AgentProcessPolicy{Ask: []string{"git push*"}}},
		},
	}
}

func (w *sessionWorld) get(name string) model.AgentSession {
	w.mu.Lock()
	defer w.mu.Unlock()
	return *w.sessions[name]
}

func waitCreated(t *testing.T, a *Agent, key string) {
	t.Helper()
	v, ok := a.sessions.jobs.Load(key)
	if !ok {
		t.Fatal("no create running")
	}
	select {
	case <-v.(*sessionCreate).done:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not finish")
	}
}

func TestAgentSessionCreateThenRunning(t *testing.T) {
	a, w := newSessionWorld(t, scheduledSession("s1"))
	w.flux = map[string]map[string]any{}
	a.reconcileAgentSessions(context.Background())
	s := w.get("s1")
	if !model.HasFinalizerList(s.Metadata.Finalizers, model.FinalizerAgentSession) {
		t.Fatalf("finalizers = %v", s.Metadata.Finalizers)
	}
	if s.Status.Phase != model.AgentSessionCreating || s.Status.CreateRequestedAt == nil {
		t.Fatalf("status = %+v, want Creating with createRequestedAt", s.Status)
	}
	waitCreated(t, a, "prod/s1")
	if w.createBody["runtime"].(map[string]any)["memory_mb"].(float64) != 4096 {
		t.Fatalf("create body = %v", w.createBody)
	}
	if ask := w.createBody["policy"].(map[string]any)["process"].(map[string]any)["ask"].([]any); len(ask) != 1 || ask[0] != "git push*" {
		t.Fatalf("create body process = %v", w.createBody["policy"])
	}
	a.reconcileAgentSessions(context.Background())
	s = w.get("s1")
	if s.Status.Phase != model.AgentSessionRunning || s.Status.SessionID == "" || s.Status.Backend != "vz" {
		t.Fatalf("status = %+v, want Running", s.Status)
	}
	if strings.Contains(s.Status.Workspace.Git, "s3cret") {
		t.Fatalf("workspace git not redacted: %q", s.Status.Workspace.Git)
	}
	if len(s.Status.Checkpoints) != 1 || s.Status.Usage == nil || s.Status.Usage.Execs != 3 || !s.Status.Capabilities.Fork {
		t.Fatalf("status = %+v", s.Status)
	}
	if w.creates != 1 {
		t.Fatalf("creates = %d", w.creates)
	}
	if w.nodeLabels[model.LabelAgentSessionsCapable] != "true" {
		t.Fatalf("node labels = %v", w.nodeLabels)
	}
}

func TestAgentSessionCapabilityNeedsMacHost(t *testing.T) {
	a, w := newSessionWorld(t)
	agentSessionHostOS = "linux"
	a.reconcileAgentSessions(context.Background())
	if v, ok := w.nodeLabels[model.LabelAgentSessionsCapable]; ok && v != nil {
		t.Fatalf("label = %v on a Linux host, want unset", v)
	}
}

func TestAgentSessionAdoptsOrphanAfterRestart(t *testing.T) {
	requested := time.Now().Add(-10 * time.Second).UTC()
	s := scheduledSession("s1")
	s.Metadata.Finalizers = []string{model.FinalizerAgentSession}
	s.Status = model.AgentSessionStatus{Phase: model.AgentSessionCreating, NodeName: "mac-1", CreateRequestedAt: &requested}
	a, w := newSessionWorld(t, s)
	w.flux["22222222-0000-0000-0000-000000000002"] = fluxSession("22222222-0000-0000-0000-000000000002", "running", time.Now())
	a.reconcileAgentSessions(context.Background())
	got := w.get("s1")
	if got.Status.SessionID != "22222222-0000-0000-0000-000000000002" || got.Status.Phase != model.AgentSessionRunning {
		t.Fatalf("status = %+v, want the orphan adopted", got.Status)
	}
	if w.creates != 0 {
		t.Fatal("a restarted node must not create a second session")
	}
}

func TestAgentSessionWaitsForInFlightCreate(t *testing.T) {
	requested := time.Now().Add(-10 * time.Second).UTC()
	s := scheduledSession("s1")
	s.Metadata.Finalizers = []string{model.FinalizerAgentSession}
	s.Status = model.AgentSessionStatus{Phase: model.AgentSessionCreating, NodeName: "mac-1", CreateRequestedAt: &requested}
	a, w := newSessionWorld(t, s)
	a.reconcileAgentSessions(context.Background())
	if w.creates != 0 || w.get("s1").Status.Phase != model.AgentSessionCreating {
		t.Fatal("within the adopt grace a restarted node waits instead of creating")
	}
}

func TestAgentSessionAdoptsForkChild(t *testing.T) {
	s := scheduledSession("child")
	s.Metadata.Annotations = map[string]string{model.AnnotationAdoptSession: "33333333-0000-0000-0000-000000000003"}
	a, w := newSessionWorld(t, s)
	w.flux["33333333-0000-0000-0000-000000000003"] = fluxSession("33333333-0000-0000-0000-000000000003", "running", time.Now())
	a.reconcileAgentSessions(context.Background())
	got := w.get("child")
	if got.Status.Phase != model.AgentSessionRunning || got.Status.SessionID != "33333333-0000-0000-0000-000000000003" || w.creates != 0 {
		t.Fatalf("status = %+v creates=%d", got.Status, w.creates)
	}
}

func TestAgentSessionEnded(t *testing.T) {
	for _, state := range []string{"expired", "deleted"} {
		s := scheduledSession("s1")
		s.Metadata.Finalizers = []string{model.FinalizerAgentSession}
		s.Status = model.AgentSessionStatus{Phase: model.AgentSessionRunning, SessionID: "44444444-0000-0000-0000-000000000004"}
		a, w := newSessionWorld(t, s)
		w.flux["44444444-0000-0000-0000-000000000004"] = fluxSession("44444444-0000-0000-0000-000000000004", state, time.Now())
		a.reconcileAgentSessions(context.Background())
		if got := w.get("s1"); got.Status.Phase != model.AgentSessionEnded || got.Status.State != state {
			t.Fatalf("%s: status = %+v", state, got.Status)
		}
	}
	s := scheduledSession("s1")
	s.Metadata.Finalizers = []string{model.FinalizerAgentSession}
	s.Status = model.AgentSessionStatus{Phase: model.AgentSessionRunning, SessionID: "55555555-0000-0000-0000-000000000005"}
	a, w := newSessionWorld(t, s)
	a.reconcileAgentSessions(context.Background())
	if got := w.get("s1"); got.Status.Phase != model.AgentSessionEnded || got.Status.State != "gone" {
		t.Fatalf("missing session: status = %+v", got.Status)
	}
}

func TestAgentSessionDeleteReleasesFinalizer(t *testing.T) {
	now := time.Now()
	s := scheduledSession("s1")
	s.Metadata.DeletionTimestamp = &now
	s.Metadata.Finalizers = []string{model.FinalizerAgentSession}
	s.Status = model.AgentSessionStatus{Phase: model.AgentSessionRunning, SessionID: "66666666-0000-0000-0000-000000000006"}
	a, w := newSessionWorld(t, s)
	w.flux["66666666-0000-0000-0000-000000000006"] = fluxSession("66666666-0000-0000-0000-000000000006", "running", time.Now())
	a.reconcileAgentSessions(context.Background())
	if len(w.deletes) != 1 || w.deletes[0] != "66666666-0000-0000-0000-000000000006" {
		t.Fatalf("deletes = %v", w.deletes)
	}
	if fins := w.get("s1").Metadata.Finalizers; len(fins) != 0 {
		t.Fatalf("finalizers = %v", fins)
	}
}

func TestAgentSessionDeleteWaitsForCreate(t *testing.T) {
	a, w := newSessionWorld(t, scheduledSession("s1"))
	w.block = make(chan struct{})
	a.reconcileAgentSessions(context.Background())
	now := time.Now()
	w.mu.Lock()
	w.sessions["s1"].Metadata.DeletionTimestamp = &now
	w.mu.Unlock()
	a.reconcileAgentSessions(context.Background())
	if fins := w.get("s1").Metadata.Finalizers; len(fins) != 1 {
		t.Fatal("the finalizer must hold while FluxVM is still creating")
	}
	close(w.block)
	waitCreated(t, a, "prod/s1")
	a.reconcileAgentSessions(context.Background())
	if len(w.deletes) != 1 || len(w.get("s1").Metadata.Finalizers) != 0 {
		t.Fatalf("deletes = %v finalizers = %v", w.deletes, w.get("s1").Metadata.Finalizers)
	}
}

func TestAgentSessionPendingWhenUnsupported(t *testing.T) {
	a, w := newSessionWorld(t, scheduledSession("s1"))
	a.sessions.probed, a.sessions.probedAt, a.sessions.supported = true, time.Now(), false
	a.reconcileAgentSessions(context.Background())
	if got := w.get("s1"); got.Status.Phase != model.AgentSessionPending || w.creates != 0 {
		t.Fatalf("status = %+v", got.Status)
	}
}
