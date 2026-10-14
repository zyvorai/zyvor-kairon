// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
	"github.com/zyvorai/kairon/internal/model"
)

func TestSessionMCPTools(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	var created model.AgentSession
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotBody = nil
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/dev/agentsessions":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		case strings.HasPrefix(r.URL.Path, "/api/v1/agentsessions/"):
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = io.WriteString(w, `{"exit_code":0,"stdout":"ok"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "t")
	withKube := func(ctx context.Context, timeout time.Duration, fn func(context.Context, *kube.Client) (string, error)) (string, error) {
		kc, err := kube.New(srv.URL, "", "", false)
		if err != nil {
			return "", err
		}
		return fn(ctx, kc)
	}
	tools := map[string]mcp.Tool{}
	for _, tool := range sessionTools("dev", withKube) {
		tools[tool.Name] = tool
	}
	call := func(name, args string) (string, error) {
		return tools[name].Call(context.Background(), json.RawMessage(args))
	}

	out, err := call("session_exec", `{"name":"work","command":"go test ./...","timeoutSeconds":60}`)
	if err != nil || !strings.Contains(out, `"stdout": "ok"`) {
		t.Fatalf("session_exec = %q, %v", out, err)
	}
	if gotPath != "/api/v1/agentsessions/dev/work/exec" || gotBody["command"] != "go test ./..." || gotBody["timeout_seconds"] != float64(60) {
		t.Fatalf("exec request %s %v", gotPath, gotBody)
	}
	if _, err := call("session_exec", `{"name":"work","command":"ls","bogus":1}`); err == nil {
		t.Fatal("unknown argument accepted")
	}
	if _, err := call("session_trajectory", `{"name":"work","namespace":"other"}`); err != nil || gotPath != "/api/v1/agentsessions/other/work/trajectory" {
		t.Fatalf("trajectory %s %v", gotPath, err)
	}
	if _, err := call("session_job_read", `{"name":"work","job":"j1","offset":42}`); err != nil || gotPath != "/api/v1/agentsessions/dev/work/jobs/j1?offset=42" {
		t.Fatalf("job read %s %v", gotPath, err)
	}
	if _, err := call("session_git", `{"name":"work","view":"diff"}`); err != nil || gotPath != "/api/v1/agentsessions/dev/work/git/diff" {
		t.Fatalf("git %s %v", gotPath, err)
	}
	if _, err := call("session_create", `{"name":"new","agent":"claude","preset":"offline","ttlSeconds":600}`); err != nil {
		t.Fatal(err)
	}
	if created.Metadata.Name != "new" || created.Spec.Agent != "claude" || created.Spec.TTLSeconds != 600 || created.Spec.Policy.Preset != "offline" {
		t.Fatalf("created = %+v", created)
	}
	for name, tool := range tools {
		read := name == "session_list" || name == "session_get" || name == "session_job_read" || name == "session_read_file" || name == "session_git" || name == "session_trajectory"
		if tool.Write == read {
			t.Errorf("%s: Write = %v", name, tool.Write)
		}
	}
}
