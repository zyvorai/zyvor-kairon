// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentPlaneKubeTools(t *testing.T) {
	const api = "/apis/kairon.zyvor.dev/v1/namespaces/default/"
	var mu sync.Mutex
	var created, deleted, policies []string
	bound := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, api)
		switch {
		case r.Method == http.MethodGet && path == "machineclaims":
			_, _ = io.WriteString(w, `{"items":[
				{"metadata":{"name":"mine","namespace":"default"},"spec":{"poolName":"agents","labels":{"kairon.zyvor.dev/tenant":"acme"}}},
				{"metadata":{"name":"theirs","namespace":"default"},"spec":{"poolName":"agents","labels":{"kairon.zyvor.dev/tenant":"other"}}}]}`)
		case r.Method == http.MethodGet && path == "machineclaims/mine":
			_, _ = io.WriteString(w, `{"metadata":{"name":"mine","namespace":"default"},"spec":{"poolName":"agents","ttlSeconds":60,"labels":{"kairon.zyvor.dev/tenant":"acme"}},"status":{"phase":"Bound","machineName":"m1","boundAt":"`+bound+`"}}`)
		case r.Method == http.MethodGet && path == "machineclaims/theirs":
			_, _ = io.WriteString(w, `{"metadata":{"name":"theirs","namespace":"default"},"spec":{"poolName":"agents","labels":{"kairon.zyvor.dev/tenant":"other"}}}`)
		case r.Method == http.MethodGet && path == "machines":
			_, _ = io.WriteString(w, `{"items":[]}`)
		case r.Method == http.MethodGet && path == "machines/m1":
			_, _ = io.WriteString(w, `{"metadata":{"name":"m1","namespace":"default","labels":{"kairon.zyvor.dev/tenant":"acme"}},"status":{"phase":"Failed","message":"image digest mismatch"}}`)
		case r.Method == http.MethodGet && path == "machines/m2":
			_, _ = io.WriteString(w, `{"metadata":{"name":"m2","namespace":"default","labels":{"kairon.zyvor.dev/tenant":"other"}},"status":{"phase":"Failed"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/default/events":
			_, _ = io.WriteString(w, `{"items":[{"type":"Warning","reason":"BackOff","message":"boot retry","count":3}]}`)
		case r.Method == http.MethodPost && path == "machineclaims":
			b, _ := io.ReadAll(r.Body)
			created = append(created, string(b))
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "machineclaims/"):
			deleted = append(deleted, strings.TrimPrefix(path, "machineclaims/"))
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && path == "machinenetworkpolicies/web":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
		case r.Method == http.MethodPost && path == "machinenetworkpolicies":
			b, _ := io.ReadAll(r.Body)
			policies = append(policies, string(b))
			_, _ = w.Write(b)
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_MCP_TENANT", "acme")
	t.Setenv("KAIRON_LLM_URL", "")

	got := callMCP(t, true, srv.URL,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_claims","arguments":{"namespace":"default"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"describe_claim","arguments":{"name":"theirs"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_sealed_claim","arguments":{"name":"job-9","pool":"agents","tenant":"acme","ttlSeconds":600,"snapshotOnRelease":true,"egress":{"allowFqdns":["pypi.org"]}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"create_sealed_claim","arguments":{"name":"job-8","pool":"agents","tenant":"acme","ttlSeconds":600,"egress":{}}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"apply_network_policy","arguments":{"intent":{"name":"web","tenant":"acme","allowFqdns":["registry.internal"]}}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"apply_claim_step","arguments":{"name":"mine"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"apply_network_policy","arguments":{"intent":{"name":"open","allowFqdns":["*"]}}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"diagnose","arguments":{"ref":"machine/m1"}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"diagnose","arguments":{"ref":"m2"}}}`,
	)
	if !strings.Contains(got["8"], "image digest mismatch") || !strings.Contains(got["8"], "BackOff (x3)") {
		t.Fatalf("diagnose: %s", got["8"])
	}
	if !strings.HasPrefix(got["9"], "tool-error:") {
		t.Fatalf("diagnose of another tenant's machine must fail: %s", got["9"])
	}
	if !strings.Contains(got["1"], `"mine"`) || strings.Contains(got["1"], `"theirs"`) {
		t.Fatalf("list_claims must hide other tenants: %s", got["1"])
	}
	if !strings.HasPrefix(got["2"], "tool-error:") {
		t.Fatalf("describe of another tenant's claim must fail: %s", got["2"])
	}
	if len(created) != 1 || !strings.Contains(created[0], `snapshot-on-release":"true"`) || !strings.Contains(created[0], `"allowFqdns":["pypi.org"]`) {
		t.Fatalf("create_sealed_claim body = %v (%s)", created, got["3"])
	}
	if !strings.HasPrefix(got["4"], "tool-error:") {
		t.Fatalf("empty egress must be refused: %s", got["4"])
	}
	if len(policies) != 1 || !strings.Contains(policies[0], `"defaultAllow":false`) {
		t.Fatalf("apply_network_policy body = %v", policies)
	}
	if !strings.Contains(got["6"], `"applied": true`) || len(deleted) != 1 || deleted[0] != "mine" {
		t.Fatalf("apply_claim_step on an expired claim: %s deleted=%v", got["6"], deleted)
	}
	if !strings.HasPrefix(got["7"], "tool-error:") {
		t.Fatalf("wildcard intent must be refused: %s", got["7"])
	}
}
