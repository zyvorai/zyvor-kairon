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

	chartpkg "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"

	"github.com/zyvorai/kairon/internal/kube"
)

func TestExtractOutput(t *testing.T) {
	cases := []struct {
		in     []string
		format string
		rest   string
		bad    bool
	}{
		{[]string{"machines"}, "", "machines", false},
		{[]string{"machines", "-o", "json"}, "json", "machines", false},
		{[]string{"-o=yaml", "machines", "--selector", "a=b"}, "yaml", "machines --selector a=b", false},
		{[]string{"machines", "--output=name"}, "name", "machines", false},
		{[]string{"machines", "-o", "wide"}, "", "", true},
		{[]string{"machines", "-o"}, "", "", true},
	}
	for _, c := range cases {
		f, rest, err := extractOutput(c.in)
		if (err != nil) != c.bad {
			t.Fatalf("%v: err=%v", c.in, err)
		}
		if err == nil && (f != c.format || strings.Join(rest, " ") != c.rest) {
			t.Errorf("%v: got %q %q", c.in, f, strings.Join(rest, " "))
		}
	}
}

func TestResourcePluralCoversEveryGetKind(t *testing.T) {
	for _, r := range append([]string{"vm", "node", "claims", "networkpolicy"}, completionKinds...) {
		if _, _, ok := resourcePlural(r); !ok {
			t.Errorf("completion kind %q has no plural mapping", r)
		}
	}
	if _, core, _ := resourcePlural("nodes"); !core {
		t.Error("nodes must be a core resource")
	}
}

func machinesServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kairon.zyvor.dev/v1/namespaces/default/machines":
			if sel := r.URL.Query().Get("labelSelector"); sel != "" && sel != "tier=web" {
				t.Errorf("unexpected selector %q", sel)
			}
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"web-1","namespace":"default","labels":{"tier":"web"}},"spec":{"nodeName":"n1"},"status":{"phase":"Running","resourceUsage":{"cpuPercent":12.5,"memoryBytes":1048576}}},
{"metadata":{"name":"web-2","namespace":"default","labels":{"tier":"web"}},"spec":{},"status":{"phase":"Running"}}]}`)
		case "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web-1":
			_, _ = io.WriteString(w, `{"metadata":{"name":"web-1"},"spec":{"nodeName":"n1"}}`)
		case "/apis/kairon.zyvor.dev/v1/namespaces/default/machinepools":
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"agents"}}]}`)
		case "/apis/kairon.zyvor.dev/v1/machines":
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"web-1","namespace":"default"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","resourceUsage":{"cpuPercent":12.5,"memoryBytes":1048576}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	clearKubeEnv(t)
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	return srv
}

func TestGetStructuredOutput(t *testing.T) {
	machinesServer(t)
	out := captureStdout(t, func() {
		if _, err := runRoot(t, "get", "machines", "--selector", "tier=web", "-o", "json"); err != nil {
			t.Error(err)
		}
	})
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("json: %v\n%s", err, out)
	}
	out = captureStdout(t, func() { _, _ = runRoot(t, "get", "vms", "-o", "yaml") })
	if !strings.Contains(out, "name: web-1") || strings.Contains(out, `"items"`) {
		t.Fatalf("yaml:\n%s", out)
	}
	out = captureStdout(t, func() { _, _ = runRoot(t, "get", "machines", "-o", "name") })
	if out != "machines.kairon.zyvor.dev/web-1\nmachines.kairon.zyvor.dev/web-2\n" {
		t.Fatalf("name: %q", out)
	}
}

func TestDescribeAndTopStructured(t *testing.T) {
	machinesServer(t)
	out := captureStdout(t, func() { _, _ = runRoot(t, "describe", "machine", "web-1", "-o", "yaml") })
	if !strings.Contains(out, "nodeName: n1") {
		t.Fatalf("describe yaml:\n%s", out)
	}
	out = captureStdout(t, func() { _, _ = runRoot(t, "top", "machines", "-o", "json") })
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("top json: %v\n%s", err, out)
	}
	if rows[0]["cpuPercent"] != 12.5 || rows[1]["cpuPercent"] != nil {
		t.Fatalf("usage mapping: %v", rows)
	}
}

func TestDynamicCompletion(t *testing.T) {
	machinesServer(t)
	out, err := runRoot(t, "__complete", "get", "mach")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "machinepools") || !strings.Contains(out, "machines") || strings.Contains(out, "quotas") {
		t.Fatalf("kind completion:\n%s", out)
	}
	out, _ = runRoot(t, "__complete", "get", "machines", "web-")
	if !strings.Contains(out, "web-1") || !strings.Contains(out, "web-2") {
		t.Fatalf("name completion:\n%s", out)
	}
	out, _ = runRoot(t, "__complete", "start", "")
	if !strings.Contains(out, "web-1") {
		t.Fatalf("machine verb completion:\n%s", out)
	}
	out, _ = runRoot(t, "__complete", "claim", "")
	if !strings.Contains(out, "agents") || strings.Contains(out, "web-1") {
		t.Fatalf("claim completes pools:\n%s", out)
	}
	// An unreachable cluster completes nothing and prints no error text.
	t.Setenv("KAIRON_KUBE_URL", "http://127.0.0.1:1")
	out, _ = runRoot(t, "__complete", "get", "machines", "")
	if strings.Contains(strings.ToLower(out), "error") || strings.Contains(out, "connection") {
		t.Fatalf("completion leaked an error:\n%s", out)
	}
}

func TestUIProxyIsConfinedToTheService(t *testing.T) {
	var gotPath, gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "dashboard")
	}))
	defer up.Close()
	kc := &kube.Client{BaseURL: up.URL, Token: "apiserver-token", HTTP: up.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, stop, err := startUIProxy(ctx, kc, &uiOpts{Namespace: "kairon-system", Service: "kairon-ui", SvcPort: 18082})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("proxy must bind loopback, got %s", addr)
	}
	const prefix = "/api/v1/namespaces/kairon-system/services/https:kairon-ui:18082/proxy"

	get := func(path, host string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		if host != "" {
			req.Host = host
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := get("/api/v1/overview", "", map[string]string{"Authorization": "Bearer browser-token-should-not-leak"}); code != 200 || body != "dashboard" {
		t.Fatalf("proxy: %d %q", code, body)
	}
	if gotPath != prefix+"/api/v1/overview" || gotAuth != "Bearer apiserver-token" {
		t.Fatalf("upstream got path=%q auth=%q", gotPath, gotAuth)
	}
	// Traversal cannot leave the service prefix.
	_, _ = get("/../../../../api/v1/secrets", "", nil)
	if !strings.HasPrefix(gotPath, prefix+"/") || strings.Contains(gotPath, "..") {
		t.Fatalf("traversal escaped the prefix: %q", gotPath)
	}
	// DNS-rebinding style Host headers are refused.
	if code, _ := get("/", "evil.example:80", nil); code != http.StatusForbidden {
		t.Fatalf("foreign Host must be refused, got %d", code)
	}
}

func TestParseDropRecords(t *testing.T) {
	cases := []struct {
		name, body string
		want       map[string]int // reason -> total count
	}{
		{"array", `[{"reason":"policy_deny","count":3},{"reason":"policy_deny"},{"reason":"spoof_ip","packets":5}]`, map[string]int{"policy_deny": 4, "spoof_ip": 5}},
		{"wrapped", `{"drops":[{"drop_reason":"sni_deny","policyName":"p1"}]}`, map[string]int{"sni_deny": 1}},
		{"map", `{"policy_deny":7,"rate_limit":2}`, map[string]int{"policy_deny": 7, "rate_limit": 2}},
		{"junk", `not json`, map[string]int{}},
	}
	for _, c := range cases {
		got := map[string]int{}
		for _, r := range parseDropRecords([]byte(c.body)) {
			got[r.Reason] += r.Count
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: %s=%d want %d", c.name, k, got[k], v)
			}
		}
	}
}

func TestNetworkObserveAggregates(t *testing.T) {
	ui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/web-1/network-drops"):
			_, _ = io.WriteString(w, `[{"reason":"policy_deny","policy":"edge","count":4},{"reason":"spoof_ip","count":1}]`)
		case strings.Contains(r.URL.Path, "/web-2/network-drops"):
			_, _ = io.WriteString(w, `[{"reason":"policy_deny","policy":"edge","count":2}]`)
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer ui.Close()
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/default/machines" {
			_, _ = io.WriteString(w, `{"items":[
{"metadata":{"name":"web-1","namespace":"default"},"status":{"phase":"Running"}},
{"metadata":{"name":"web-2","namespace":"default"},"status":{"phase":"Running"}},
{"metadata":{"name":"db-1","namespace":"default"},"status":{"phase":"Running"}},
{"metadata":{"name":"idle","namespace":"default"},"status":{"phase":"Stopped"}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer kube.Close()
	clearKubeEnv(t)
	t.Setenv("KAIRON_KUBE_URL", kube.URL)
	t.Setenv("KAIRON_UI_URL", ui.URL)

	out, err := runRoot(t, "network", "observe", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var sum observeSummary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if sum.TotalDrops != 7 || sum.MachinesSeen != 2 || sum.MachinesError != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if len(sum.Rows) != 2 || sum.Rows[0].Key != "policy_deny" || sum.Rows[0].Drops != 6 || sum.Rows[0].Machines != 2 {
		t.Fatalf("rows by reason: %+v", sum.Rows)
	}
	out, _ = runRoot(t, "network", "observe", "--by", "policy", "-o", "json")
	_ = json.Unmarshal([]byte(out), &sum)
	if sum.Rows[0].Key != "edge" || sum.Rows[0].Drops != 6 || sum.Rows[1].Key != "(unknown)" {
		t.Fatalf("rows by policy: %+v", sum.Rows)
	}
	out, _ = runRoot(t, "network", "observe", "--by", "machine", "--top", "1", "-o", "json")
	_ = json.Unmarshal([]byte(out), &sum)
	if len(sum.Rows) != 1 || sum.Rows[0].Key != "default/web-1" {
		t.Fatalf("rows by machine: %+v", sum.Rows)
	}
	if _, err := runRoot(t, "network", "observe", "--by", "nonsense"); err == nil {
		t.Fatal("bad --by accepted")
	}
	t.Setenv("KAIRON_UI_URL", "")
	if _, err := runRoot(t, "network", "observe"); err == nil || !strings.Contains(err.Error(), "KAIRON_UI_URL") {
		t.Fatalf("missing UI URL must be reported, got %v", err)
	}
}

func TestSemver(t *testing.T) {
	if c, ok := compareSemver("v0.8.0", "0.9.0"); !ok || c != -1 {
		t.Fatalf("%d %v", c, ok)
	}
	if c, ok := compareSemver("0.8.10", "0.8.9"); !ok || c != 1 {
		t.Fatalf("numeric, not lexical: %d %v", c, ok)
	}
	if c, ok := compareSemver("0.9.0-rc1", "0.9.0"); !ok || c != 0 {
		t.Fatalf("prerelease suffix ignored: %d %v", c, ok)
	}
	if _, ok := compareSemver("dev", "0.9.0"); ok {
		t.Fatal("non-semver compared")
	}
	if !minorBump("0.8.2", "0.9.0") || minorBump("0.8.1", "0.8.2") {
		t.Fatal("minorBump")
	}
}

func TestUpgradeCheck(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v0.9.0"}`)
	}))
	defer api.Close()
	t.Setenv("KAIRON_RELEASE_API", api.URL)
	old := getReleaseFn
	defer func() { getReleaseFn = old }()
	getReleaseFn = func(*releaseRef) (*release.Release, error) {
		return &release.Release{Name: "kairon", Chart: &chartpkg.Chart{Metadata: &chartpkg.Metadata{Name: "kairon", Version: "0.7.2"}}}, nil
	}
	chart := writeTestChart(t, "0.8.0")
	out, err := runRoot(t, "upgrade", "--check", "--chart", chart)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Installed chart:      0.7.2", "Chart to apply:       0.8.0", "Latest release:       v0.9.0",
		"newer release (v0.9.0)", "Upgrade available: 0.7.2 -> 0.9.0", "apply the CRDs", "deploy/crd.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A feed that cannot be reached must not fail the check.
	t.Setenv("KAIRON_RELEASE_API", "http://127.0.0.1:1")
	out, err = runRoot(t, "upgrade", "--check", "--chart", chart)
	if err != nil || !strings.Contains(out, "unavailable") || !strings.Contains(out, "Upgrade available: 0.7.2 -> 0.8.0") {
		t.Fatalf("offline check: %v\n%s", err, out)
	}
}
