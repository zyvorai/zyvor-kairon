// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/rehearsal"
)

func TestRehearsalCommandAndGuestExecution(t *testing.T) {
	cmd := NewRootCmd(nil)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"rehearsal", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("cleanup")) || !bytes.Contains(out.Bytes(), []byte("run")) {
		t.Fatal("rehearsal commands missing")
	}
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{{"success", `{"exitCode":0,"stdout":"ok"}`, 200, false}, {"guest failure", `{"exitCode":7}`, 200, false}, {"omitted exit", `{}`, 200, true}, {"denied", `{"error":"denied"}`, 403, true}, {"malformed", `!`, 200, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/machines/default/copy/exec" || r.Method != "POST" {
					t.Errorf("wrong route: %s %s", r.Method, r.URL.Path)
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req["path"] != "/bin/true" {
					t.Error("command not forwarded")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			t.Setenv("KAIRON_UI_URL", srv.URL)
			result, err := rehearsalExec(context.Background(), "default", "copy", rehearsal.Step{Path: "/bin/true", TimeoutSeconds: 5})
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.name == "guest failure" && result.ExitCode != 7 {
				t.Fatal("guest exit status lost")
			}
		})
	}
}
