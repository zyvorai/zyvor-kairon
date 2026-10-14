// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestAgentSessionSpecFromFlags(t *testing.T) {
	spec, err := agentSessionSpec(sessionRunFlags{
		agent: "codex", git: "https://github.com/org/repo", memory: "4Gi",
		preset: "developer-safe", allowHosts: []string{"api.github.com", "*.pypi.org"},
		processDeny: []string{"rm -rf *"}, ttl: 2 * time.Hour,
		budget: model.AgentBudget{MaxExecs: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Workspace == nil || spec.Workspace.Git != "https://github.com/org/repo" || spec.TTLSeconds != 7200 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Policy == nil || spec.Policy.Network == nil || len(spec.Policy.Network.Allow) != 2 || spec.Policy.Process.Deny[0] != "rm -rf *" {
		t.Fatalf("policy = %+v", spec.Policy)
	}
	if spec.Budget == nil || spec.Budget.MaxExecs != 100 {
		t.Fatalf("budget = %+v", spec.Budget)
	}
	if s, _ := agentSessionSpec(sessionRunFlags{agent: "a"}); s.Policy != nil || s.Budget != nil || s.Workspace != nil {
		t.Fatalf("empty flags must leave policy, budget and workspace unset: %+v", s)
	}
	if _, err := agentSessionSpec(sessionRunFlags{preset: "yolo"}); err == nil || !strings.HasPrefix(err.Error(), "spec.") {
		t.Fatalf("bad preset = %v", err)
	}
}

const fluxctlTrajectory = `{
  "session": {"id": "4de0d044-b7a3-46e3-b2c7-f6279110cabd", "agent": "custom",
              "backend": "vz", "state": "running", "sandbox_id": "x",
              "created_at": "2026-10-13T09:30:00Z"},
  "steps": [
    {"seq": 0, "ts": "2026-10-13T09:30:10Z", "source": "vm", "kind": "vm.create", "detail": {"name": "a"}},
    {"seq": 1, "ts": "2026-10-13T09:30:12.500Z", "source": "ledger", "kind": "exec",
     "detail": {"command": "ls", "exit_code": 2, "duration_ms": 500}},
    {"seq": 2, "ts": "2026-10-13T09:30:13Z", "source": "ledger", "kind": "exec.refused", "detail": {"command": "rm"}}
  ]
}`

// The fluxctl test case (crates/fluxctl/src/agent_session.rs,
// a_trajectory_becomes_one_trace), with span ids computed independently.
func TestOTLPTracesMatchesFluxctl(t *testing.T) {
	doc, err := otlpTraces([]byte(fluxctlTrajectory))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	var out struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope struct {
					Name string `json:"name"`
				} `json:"scope"`
				Spans []struct {
					TraceID      string           `json:"traceId"`
					SpanID       string           `json:"spanId"`
					ParentSpanID string           `json:"parentSpanId"`
					Name         string           `json:"name"`
					Start        string           `json:"startTimeUnixNano"`
					End          string           `json:"endTimeUnixNano"`
					Attributes   []map[string]any `json:"attributes"`
					Status       struct {
						Code int `json:"code"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	ss := out.ResourceSpans[0].ScopeSpans[0]
	if ss.Scope.Name != "fluxctl agent export" {
		t.Fatalf("scope = %q", ss.Scope.Name)
	}
	spans := ss.Spans
	if len(spans) != 4 || spans[0].Name != "agent.session" || spans[0].TraceID != "4de0d044b7a346e3b2c7f6279110cabd" {
		t.Fatalf("spans = %+v", spans)
	}
	wantIDs := []string{"94fd258d735d9909", "a4b3b0313b0a8467", "7db182b8481c9dca", "9b4dd474f5a53149"}
	for i, s := range spans {
		if s.SpanID != wantIDs[i] {
			t.Errorf("span %d id = %s, want %s", i, s.SpanID, wantIDs[i])
		}
		if i > 0 && (s.ParentSpanID != spans[0].SpanID || s.TraceID != spans[0].TraceID) {
			t.Errorf("span %d not under the root", i)
		}
	}
	exec := spans[2]
	if exec.Start != "1791883812000000000" || exec.End != "1791883812500000000" || exec.Status.Code != 2 {
		t.Fatalf("exec span = %+v", exec)
	}
	if spans[3].Status.Code != 2 || spans[1].Status.Code != 0 {
		t.Fatal("a refused step and a non-zero exit are errors; others are not")
	}
	if spans[0].Start != "1791883810000000000" || spans[0].End != "1791883813000000000" {
		t.Fatalf("root covers %s..%s", spans[0].Start, spans[0].End)
	}
	attrs, _ := json.Marshal(exec.Attributes)
	for _, want := range []string{
		`{"key":"fluxvm.source","value":{"stringValue":"ledger"}}`,
		`{"key":"fluxvm.step","value":{"intValue":"1"}}`,
		`{"key":"fluxvm.command","value":{"stringValue":"ls"}}`,
		`{"key":"fluxvm.duration_ms","value":{"intValue":"500"}}`,
		`{"key":"fluxvm.exit_code","value":{"intValue":"2"}}`,
	} {
		if !strings.Contains(string(attrs), want) {
			t.Errorf("exec attributes %s lack %s", attrs, want)
		}
	}
	if _, err := otlpTraces([]byte(`{"session":{"id":"nope"},"steps":[]}`)); err == nil {
		t.Fatal("a non-UUID session id was accepted")
	}
	empty, err := otlpTraces([]byte(`{"session":{"id":"4de0d044-b7a3-46e3-b2c7-f6279110cabd","created_at":"2026-10-13T09:30:00Z"},"steps":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	root := empty["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]map[string]any)[0]
	if root["startTimeUnixNano"] != root["endTimeUnixNano"] || root["startTimeUnixNano"] != "1791883800000000000" {
		t.Fatalf("empty trajectory root = %v", root)
	}
}

func TestTrajectoryJSONL(t *testing.T) {
	var buf bytes.Buffer
	if err := trajectoryJSONL([]byte(fluxctlTrajectory), &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 || !strings.Contains(lines[0], `"type":"session"`) || !strings.Contains(lines[0], `"sandbox_id":"x"`) {
		t.Fatalf("jsonl = %q", buf.String())
	}
	if lines[1] != `{"seq":0,"ts":"2026-10-13T09:30:10Z","source":"vm","kind":"vm.create","detail":{"name":"a"}}` {
		t.Fatalf("step line = %s", lines[1])
	}
}

func TestSendOTLP(t *testing.T) {
	var gotPath, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTenant = r.URL.Path, r.Header.Get("x-tenant")
	}))
	defer srv.Close()
	if err := sendOTLP(context.Background(), srv.URL+"/", []string{"x-tenant=a"}, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/traces" || gotTenant != "a" {
		t.Fatalf("path %q tenant %q", gotPath, gotTenant)
	}
	if err := sendOTLP(context.Background(), srv.URL, []string{"novalue"}, map[string]any{}); err == nil {
		t.Fatal("a header without = was accepted")
	}
}

// TestSessionRollout runs a rollout against one server that is both
// kairon-ui and the apiserver.
func TestSessionRollout(t *testing.T) {
	var mu sync.Mutex
	execs := map[string]string{}
	deleted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const kubePrefix = "/apis/kairon.zyvor.dev/v1/namespaces/dev/agentsessions/"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agentsessions/dev/work/fork":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"items":[{"id":"a"},{"id":"b"}],"total_ms":40,"agentSessions":["work-a","work-b"]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/exec"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			child := strings.Split(r.URL.Path, "/")[5]
			execs[child], _ = body["command"].(string)
			code := 0
			if child == "work-b" {
				code = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": code, "stdout": child, "stderr": ""})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, kubePrefix):
			name := strings.TrimPrefix(r.URL.Path, kubePrefix)
			_ = json.NewEncoder(w).Encode(model.AgentSession{
				Metadata: model.ObjectMeta{Name: name, Namespace: "dev"},
				Status:   model.AgentSessionStatus{Phase: model.AgentSessionRunning},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, kubePrefix):
			deleted[strings.TrimPrefix(r.URL.Path, kubePrefix)] = true
			_, _ = io.WriteString(w, "{}")
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sessionRollout(context.Background(), kc, "dev", "work", 2, "make test", 0, false, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if execs["work-a"] != "make test" || execs["work-b"] != "make test" {
		t.Fatalf("execs = %v", execs)
	}
	if !deleted["work-a"] || !deleted["work-b"] || out["children_deleted"] != 2 || out["fork_ms"] != float64(40) {
		t.Fatalf("out = %v deleted = %v", out, deleted)
	}
	results := out["results"].([]map[string]any)
	if *results[1]["exit_code"].(*int64) != 1 || results[0]["stdout"] != "work-a" {
		t.Fatalf("results = %v", results)
	}
}
