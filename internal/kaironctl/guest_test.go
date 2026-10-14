// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGuestAndRuntimeCommandsHitTheirRoutes(t *testing.T) {
	type call struct{ method, path, body string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method, r.URL.EscapedPath(), string(b)})
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "stdout": "", "ok": true})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "tok")
	tmpl := filepath.Join(t.TempDir(), "t.json")
	if err := os.WriteFile(tmpl, []byte(`{"vcpus":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		args []string
		want call
	}{
		{[]string{"guest", "exec", "web", "--", "uname", "-a"}, call{"POST", "/api/v1/machines/prod/web/exec", `{"args":["-a"],"path":"uname","timeoutSeconds":30}`}},
		{[]string{"guest", "exec", "web", "--agent", "--", "df", "-h"}, call{"POST", "/api/v1/machines/prod/web/agent-exec", `{"command":"df -h","timeoutSeconds":30}`}},
		{[]string{"guest", "firewall", "open", "web", "ssh", "22"}, call{"POST", "/api/v1/machines/prod/web/qga/firewall/open", `{"name":"ssh","port":22,"protocol":"tcp"}`}},
		{[]string{"guest", "vm-restore", "web", "before"}, call{"POST", "/api/v1/machines/prod/web/vm-restore-snapshot", `{"tag":"before"}`}},
		{[]string{"guest", "frozen", "web"}, call{"GET", "/api/v1/machines/prod/web/frozen", ""}},
		{[]string{"runtime", "catalog", "n1"}, call{"GET", "/api/v1/nodes/n1/catalog", ""}},
		{[]string{"runtime", "catalog", "rename", "n1", "a", "b"}, call{"POST", "/api/v1/nodes/n1/catalog/a/rename", `{"newName":"b"}`}},
		{[]string{"runtime", "catalog", "delete", "n1", "a"}, call{"DELETE", "/api/v1/nodes/n1/catalog/a", ""}},
		{[]string{"runtime", "pools", "create", "n1", "p", "--size", "2", "--template", tmpl}, call{"POST", "/api/v1/nodes/n1/pools", `{"name":"p","size":2,"template":{"vcpus":1}}`}},
		{[]string{"runtime", "pools", "claim", "n1", "p", "--ttl", "60"}, call{"POST", "/api/v1/nodes/n1/pools/p/claim", `{"ttlSeconds":60}`}},
		{[]string{"runtime", "egress-check", "n1", "example.com"}, call{"POST", "/api/v1/nodes/n1/egress-check", `{"host":"example.com"}`}},
	}
	for _, c := range cases {
		got = nil
		root := NewRootCmd(&Options{Namespace: "prod"})
		root.SetArgs(append([]string{"-n", "prod"}, c.args...))
		root.SetOut(io.Discard)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if len(got) != 1 {
			t.Fatalf("%v: %d calls", c.args, len(got))
		}
		g := got[0]
		if g.method != c.want.method || g.path != c.want.path {
			t.Fatalf("%v: got %s %s", c.args, g.method, g.path)
		}
		if c.want.body != "" && !jsonEqual(g.body, c.want.body) {
			t.Fatalf("%v: body %s, want %s", c.args, g.body, c.want.body)
		}
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
