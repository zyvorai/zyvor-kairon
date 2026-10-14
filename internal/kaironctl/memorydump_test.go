// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestStartMemoryDumpPollsUntilDone(t *testing.T) {
	incidentPoll = time.Millisecond
	const base = "/api/v1/machines/prod/db/diagnostics"
	polls := 0
	var req fluxvm.DiagnosticsRequest
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST " + base:
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "state": "running"})
		case "GET " + base + "/tok":
			polls++
			state := "running"
			if polls > 1 {
				state = "done"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "state": state,
				"memory_dump": map[string]any{"path": "/d/memory.elf", "size_bytes": 2 << 20, "sha256": "ab", "paused_ms": 900}})
		case "DELETE " + base + "/tok":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "")

	job, err := startMemoryDump(context.Background(), "prod", "db")
	if err != nil {
		t.Fatal(err)
	}
	if !req.MemoryDump || !req.Redact {
		t.Fatalf("request %+v: want a redacted bundle with a memory dump", req)
	}
	if job.Token != "tok" || job.MemoryDump.Path != "/d/memory.elf" || polls != 2 {
		t.Fatalf("job %+v after %d polls", job, polls)
	}

	root := NewRootCmd(nil)
	root.SetArgs([]string{"memory-dump", "remove", "db", "tok", "-n", "prod"})
	if err := root.Execute(); err != nil || !deleted {
		t.Fatalf("remove: err=%v deleted=%v", err, deleted)
	}
}

func TestStartMemoryDumpFailed(t *testing.T) {
	incidentPoll = time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "state": "failed", "error": "not a QEMU runtime"})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	if _, err := startMemoryDump(context.Background(), "default", "web"); err == nil {
		t.Fatal("expected an error for a failed bundle")
	}
}
