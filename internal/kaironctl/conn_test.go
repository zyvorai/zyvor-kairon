// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearKubeEnv isolates a test from the developer's real cluster settings.
func clearKubeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KAIRON_KUBE_URL", "")
	t.Setenv("KAIRON_KUBE_TOKEN", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	// client-go captures the home kubeconfig path when the package is
	// initialised, so changing HOME here does not hide a real ~/.kube/config.
	// Point KUBECONFIG at a file that does not exist instead: an empty value
	// would fall back to that default file.
	home := t.TempDir()
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig"))
	t.Setenv("HOME", home)
	kubeFlags.Kubeconfig, kubeFlags.Context = "", ""
	t.Cleanup(func() { kubeFlags.Kubeconfig, kubeFlags.Context = "", "" })
}

// fakeAPIServer answers /apis/kairon.zyvor.dev/*/machines with one Machine
// named tag and records the Authorization header it saw.
func fakeAPIServer(t *testing.T, tag string, sawAuth *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sawAuth != nil {
			*sawAuth = r.Header.Get("Authorization")
		}
		if strings.HasSuffix(r.URL.Path, "/machines") {
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"`+tag+`","namespace":"default"},"status":{"phase":"Running"}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeKubeconfig(t *testing.T, current string, servers map[string]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\ncurrent-context: " + current + "\nclusters:\n")
	for name, url := range servers {
		b.WriteString("- name: " + name + "\n  cluster:\n    server: " + url + "\n    insecure-skip-tls-verify: true\n")
	}
	b.WriteString("users:\n- name: u\n  user:\n    token: tok-from-kubeconfig\ncontexts:\n")
	for name := range servers {
		b.WriteString("- name: " + name + "\n  context:\n    cluster: " + name + "\n    user: u\n")
	}
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKubeconfigDrivesRESTClient(t *testing.T) {
	clearKubeEnv(t)
	var auth string
	srv := fakeAPIServer(t, "from-kubeconfig", &auth)
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "only", map[string]string{"only": srv.URL}))

	kc, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	ms, err := kc.ListMachines(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Metadata.Name != "from-kubeconfig" {
		t.Fatalf("unexpected machines: %+v", ms)
	}
	if auth != "Bearer tok-from-kubeconfig" {
		t.Fatalf("kubeconfig credentials not sent: %q", auth)
	}
}

func TestContextAndKubeconfigFlagsBeatEnvironment(t *testing.T) {
	clearKubeEnv(t)
	a := fakeAPIServer(t, "cluster-a", nil)
	b := fakeAPIServer(t, "cluster-b", nil)
	env := fakeAPIServer(t, "from-env", nil)
	cfg := writeKubeconfig(t, "a", map[string]string{"a": a.URL, "b": b.URL})
	// An env endpoint is set, but an explicit flag must win.
	t.Setenv("KAIRON_KUBE_URL", env.URL)
	t.Setenv("KAIRON_KUBE_INSECURE", "true")

	out := captureStdout(t, func() {
		root := NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
		root.SetArgs([]string{"--kubeconfig", cfg, "--context", "b", "get", "machines"})
		if err := root.Execute(); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "cluster-b") || strings.Contains(out, "from-env") {
		t.Fatalf("--context b not honoured:\n%s", out)
	}

	// The flags are also accepted after the verb of a DisableFlagParsing command.
	kubeFlags.Kubeconfig, kubeFlags.Context = "", ""
	out = captureStdout(t, func() {
		root := NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
		root.SetArgs([]string{"get", "machines", "--kubeconfig=" + cfg, "--context", "a"})
		if err := root.Execute(); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "cluster-a") {
		t.Fatalf("trailing flags not honoured:\n%s", out)
	}
}

func TestEnvEndpointStillWorksWithoutFlags(t *testing.T) {
	clearKubeEnv(t)
	srv := fakeAPIServer(t, "from-env", nil)
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	t.Setenv("KAIRON_KUBE_INSECURE", "true")
	kc, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	if ms, err := kc.ListMachines(t.Context()); err != nil || len(ms) != 1 {
		t.Fatalf("%v %v", ms, err)
	}
}

func TestNoClusterConfiguredIsActionable(t *testing.T) {
	clearKubeEnv(t)
	_, err := newKubeClient()
	if err == nil || !strings.Contains(err.Error(), "--kubeconfig") {
		t.Fatalf("want an error naming the ways to configure a cluster, got %v", err)
	}
}

func TestExtractConnFlags(t *testing.T) {
	clearKubeEnv(t)
	got := extractConnFlags([]string{"machines", "--context", "prod", "-l", "a=b", "--kubeconfig=/k"})
	if strings.Join(got, " ") != "machines -l a=b" {
		t.Fatalf("flags not stripped: %v", got)
	}
	if kubeFlags.Context != "prod" || kubeFlags.Kubeconfig != "/k" {
		t.Fatalf("flags not recorded: %+v", kubeFlags)
	}
	// A dangling flag is left for the handler to reject.
	if got := extractConnFlags([]string{"--context"}); len(got) != 1 {
		t.Fatalf("dangling flag swallowed: %v", got)
	}
}

func TestUITokenSelection(t *testing.T) {
	t.Setenv("KAIRON_UI_URL", " http://ui:18082/ ")
	t.Setenv("KAIRON_UI_TOKEN", "")
	t.Setenv("KAIRON_CONSOLE_TOKEN", "console")
	if got := uiBaseURL(); got != "http://ui:18082" {
		t.Fatalf("base %q", got)
	}
	if uiToken(false) != "" || uiToken(true) != "console" {
		t.Fatalf("console token must only back observability: %q %q", uiToken(false), uiToken(true))
	}
	t.Setenv("KAIRON_UI_TOKEN", "ui")
	if uiToken(true) != "ui" || uiToken(false) != "ui" {
		t.Fatal("UI token must win")
	}
}

func TestKubeAPIHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/namespaces/kairon-system/pods":
			if r.URL.Query().Get("labelSelector") != "app=kairon-controller" {
				t.Errorf("selector not sent: %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"c-1","labels":{"app":"kairon-controller"}},
"spec":{"nodeName":"n1","containers":[{"name":"controller"}]},
"status":{"phase":"Running","podIP":"10.0.0.5","containerStatuses":[{"ready":true,"restartCount":2}]}}]}`)
		case strings.HasSuffix(r.URL.Path, "/pods/c-1/log"):
			q := r.URL.Query()
			if q.Get("tailLines") != "50" || q.Get("sinceSeconds") != "600" || q.Get("previous") != "true" {
				t.Errorf("log query: %v", q)
			}
			_, _ = io.WriteString(w, "line one\nline two\n")
		case r.URL.Path == "/api/v1/namespaces/kairon-system/events":
			_, _ = io.WriteString(w, `{"items":[
{"metadata":{"namespace":"kairon-system"},"type":"Warning","reason":"BackOff","message":"later","involvedObject":{"kind":"Pod","name":"c-1"},"lastTimestamp":"2026-10-11T10:05:00Z","count":3},
{"metadata":{"namespace":"kairon-system"},"type":"Normal","reason":"Pulled","message":"earlier","involvedObject":{"kind":"Pod","name":"c-1"},"lastTimestamp":"2026-10-11T10:00:00Z"}]}`)
		case r.URL.Path == "/api/v1/namespaces/kairon-system/services/http:kairon-ui:18082/proxy/readyz":
			_, _ = io.WriteString(w, `{"ready":true}`)
		case strings.HasSuffix(r.URL.Path, "/missing/log"):
			http.Error(w, `{"message":"pod not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	kc, err := newKubeClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	pods, err := listPods(ctx, kc, "kairon-system", "app=kairon-controller")
	if err != nil || len(pods) != 1 || pods[0].Name != "c-1" || !pods[0].Ready || pods[0].Restarts != 2 || pods[0].Node != "n1" {
		t.Fatalf("pods: %+v %v", pods, err)
	}

	rc, err := podLogs(ctx, kc, "kairon-system", "c-1", logOptions{Tail: 50, Since: 10 * time.Minute, Previous: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rc)
	_ = rc.Close()
	if buf.String() != "line one\nline two\n" {
		t.Fatalf("log body %q", buf.String())
	}
	if _, err := podLogs(ctx, kc, "kairon-system", "missing", logOptions{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}

	evs, err := listNamespaceEvents(ctx, kc, "kairon-system")
	if err != nil || len(evs) != 2 || evs[0].Reason != "Pulled" || evs[1].Reason != "BackOff" || evs[1].Object != "Pod/c-1" {
		t.Fatalf("events must be oldest first: %+v %v", evs, err)
	}

	body, err := proxyGet(ctx, kc, "services", "kairon-system", "http:kairon-ui:18082", "/readyz")
	if err != nil || string(body) != `{"ready":true}` {
		t.Fatalf("proxy: %q %v", body, err)
	}
	if _, err := proxyGet(ctx, kc, "nodes", "ns", "x", "/"); err == nil {
		t.Fatal("unsupported proxy kind accepted")
	}
}
