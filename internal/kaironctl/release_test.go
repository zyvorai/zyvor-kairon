// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/action"
	chartpkg "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// useMemoryHelm points every Helm SDK call at one in-memory release store
// with a printing (no-op) kube client for the duration of the test.
func useMemoryHelm(t *testing.T) {
	t.Helper()
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...any) {},
	}
	old := newActionConfigFn
	newActionConfigFn = func(string) (*action.Configuration, *cli.EnvSettings, error) {
		return cfg, cli.New(), nil
	}
	t.Cleanup(func() { newActionConfigFn = old })
}

// writeTestChart writes a minimal chart (version ver) and returns its path.
func writeTestChart(t *testing.T, ver string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kairon")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":               "apiVersion: v2\nname: kairon\nversion: " + ver + "\nappVersion: \"" + ver + "\"\n",
		"values.yaml":              "ui:\n  enabled: false\ncontroller:\n  replicaCount: 1\n",
		"values-production.yaml":   "controller:\n  replicaCount: 3\n",
		"templates/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata:\n  v: \"{{ .Values.controller.replicaCount }}\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testOpts(chart string, sets ...string) *helmInstallOpts {
	return &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Chart: chart, Sets: sets, CreateNS: true, Timeout: time.Second}
}

func userValues(t *testing.T) map[string]any {
	t.Helper()
	v, err := releaseValuesFn(&releaseRef{Name: "kairon", Namespace: "kairon-system"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func isNum(v any, n int) bool {
	switch x := v.(type) {
	case int:
		return x == n
	case int64:
		return x == int64(n)
	case float64:
		return x == float64(n)
	}
	return false
}

func TestUpgradeReusesPreviousValues(t *testing.T) {
	useMemoryHelm(t)
	chart := writeTestChart(t, "0.1.0")
	if _, err := sdkInstall(testOpts(chart, "ui.enabled=true"), false); err != nil {
		t.Fatal(err)
	}
	// An upgrade that does not restate ui.enabled must keep it.
	if _, err := sdkInstall(testOpts(chart, "controller.replicaCount=2"), true); err != nil {
		t.Fatal(err)
	}
	got := userValues(t)
	ui, _ := got["ui"].(map[string]any)
	if ui["enabled"] != true {
		t.Fatalf("ui.enabled was reset by upgrade: %v", got)
	}
	ctl, _ := got["controller"].(map[string]any)
	if !isNum(ctl["replicaCount"], 2) {
		t.Fatalf("new --set not applied: %v", got)
	}
}

func TestUpgradeResetValuesDropsPrevious(t *testing.T) {
	useMemoryHelm(t)
	chart := writeTestChart(t, "0.1.0")
	if _, err := sdkInstall(testOpts(chart, "ui.enabled=true"), false); err != nil {
		t.Fatal(err)
	}
	h := testOpts(chart, "controller.replicaCount=2")
	h.ResetValues = true
	if _, err := sdkInstall(h, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := userValues(t)["ui"]; ok {
		t.Fatalf("--reset-values kept previous values: %v", userValues(t))
	}
}

func TestConfigUnsetRemovesValue(t *testing.T) {
	useMemoryHelm(t)
	chart := writeTestChart(t, "0.1.0")
	if _, err := sdkInstall(testOpts(chart, "ui.enabled=true", "controller.replicaCount=2"), false); err != nil {
		t.Fatal(err)
	}
	if err := applyConfigChange(context.Background(), testOpts(chart, "ui.enabled=null")); err != nil {
		t.Fatal(err)
	}
	got := userValues(t)
	if ui, ok := got["ui"].(map[string]any); ok {
		if _, has := ui["enabled"]; has {
			t.Fatalf("ui.enabled not removed: %v", got)
		}
	}
	if _, ok := got["controller"]; !ok {
		t.Fatalf("unrelated value lost: %v", got)
	}
}

func TestConfigSetRefusesChartVersionChange(t *testing.T) {
	useMemoryHelm(t)
	if _, err := sdkInstall(testOpts(writeTestChart(t, "0.1.0")), false); err != nil {
		t.Fatal(err)
	}
	err := applyConfigChange(context.Background(), testOpts(writeTestChart(t, "0.2.0"), "ui.enabled=true"))
	if err == nil || !strings.Contains(err.Error(), "installed chart is 0.1.0") {
		t.Fatalf("want chart-mismatch error, got %v", err)
	}
}

func TestHistoryAndRollback(t *testing.T) {
	useMemoryHelm(t)
	chart := writeTestChart(t, "0.1.0")
	if _, err := sdkInstall(testOpts(chart, "ui.enabled=true"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := sdkInstall(testOpts(chart, "ui.enabled=false"), true); err != nil {
		t.Fatal(err)
	}
	ref := &releaseRef{Name: "kairon", Namespace: "kairon-system"}
	revs, err := releaseHistoryFn(ref)
	if err != nil || len(revs) != 2 {
		t.Fatalf("history: %v %d", err, len(revs))
	}
	var buf bytes.Buffer
	if err := writeHistory(&buf, revs, "table"); err != nil || !strings.Contains(buf.String(), "kairon-0.1.0") {
		t.Fatalf("history table: %v\n%s", err, buf.String())
	}
	if err := rollbackFn(ref, 1, false, time.Second, false); err != nil {
		t.Fatal(err)
	}
	ui, _ := userValues(t)["ui"].(map[string]any)
	if ui["enabled"] != true {
		t.Fatalf("rollback did not restore revision 1 values: %v", userValues(t))
	}
}

func TestValueReuseModes(t *testing.T) {
	cases := []struct {
		name        string
		h           helmInstallOpts
		upgradeOnly bool
		reuse       bool
		thenReuse   bool
	}{
		{"upgrade default", helmInstallOpts{}, true, false, true},
		{"install default", helmInstallOpts{}, false, false, false},
		{"upgrade reset", helmInstallOpts{ResetValues: true}, true, false, false},
		{"upgrade reuse", helmInstallOpts{ReuseValues: true}, true, true, false},
	}
	for _, c := range cases {
		reuse, thenReuse := valueReuse(&c.h, c.upgradeOnly)
		if reuse != c.reuse || thenReuse != c.thenReuse {
			t.Errorf("%s: got reuse=%v thenReuse=%v", c.name, reuse, thenReuse)
		}
	}
	args := strings.Join(buildHelmUpgradeArgs(&helmInstallOpts{ReleaseName: "kairon", Namespace: "n"}, true), " ")
	if !strings.Contains(args, "--reset-then-reuse-values") {
		t.Errorf("helm CLI upgrade args missing reuse flag: %s", args)
	}
	args = strings.Join(buildHelmUpgradeArgs(&helmInstallOpts{ReleaseName: "kairon", Namespace: "n"}, false), " ")
	if strings.Contains(args, "reuse") {
		t.Errorf("install args must not reuse values: %s", args)
	}
}

func TestWithProfile(t *testing.T) {
	chart := writeTestChart(t, "0.1.0")
	h, err := withProfile(&helmInstallOpts{Profile: "production", ValuesFiles: []string{"mine.yaml"}}, chart)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ValuesFiles) != 2 || !strings.HasSuffix(h.ValuesFiles[0], "values-production.yaml") || h.ValuesFiles[1] != "mine.yaml" {
		t.Fatalf("profile must come first so -f overrides it: %v", h.ValuesFiles)
	}
	if _, err := withProfile(&helmInstallOpts{Profile: "nope"}, chart); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if _, err := withProfile(&helmInstallOpts{Profile: "../x"}, chart); err == nil {
		t.Fatal("path traversal accepted")
	}
	if h, err := withProfile(&helmInstallOpts{}, chart); err != nil || len(h.ValuesFiles) != 0 {
		t.Fatalf("no profile must be a no-op: %v %v", h, err)
	}
}

func TestProfileAppliedOnInstall(t *testing.T) {
	useMemoryHelm(t)
	h := testOpts(writeTestChart(t, "0.1.0"))
	h.Profile = "production"
	if _, err := sdkInstall(h, false); err != nil {
		t.Fatal(err)
	}
	all, err := releaseValuesFn(&releaseRef{Name: "kairon", Namespace: "kairon-system"}, true)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := all["controller"].(map[string]any)
	if !isNum(ctl["replicaCount"], 3) {
		t.Fatalf("production profile not applied: %v", all)
	}
}

func TestVersionBareAndServer(t *testing.T) {
	var buf bytes.Buffer
	root := NewRootCmd(&Options{Name: "kaironctl", Version: "v9.9.9"})
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "v9.9.9\n" {
		t.Fatalf("bare version must be exactly the client string, got %q", buf.String())
	}

	oldRel, oldImg := getReleaseFn, workloadImagesFn
	defer func() { getReleaseFn, workloadImagesFn = oldRel, oldImg }()
	getReleaseFn = func(*releaseRef) (*release.Release, error) {
		return &release.Release{
			Name: "kairon", Namespace: "kairon-system", Version: 4,
			Info:  &release.Info{Status: release.StatusDeployed},
			Chart: &chartpkg.Chart{Metadata: &chartpkg.Metadata{Name: "kairon", Version: "0.8.0", AppVersion: "0.8.0"}},
		}, nil
	}
	workloadImagesFn = func(context.Context, string) (map[string]string, []string) {
		return map[string]string{"controller": "ghcr.io/zyvorai/kairon-controller:0.8.0"}, nil
	}
	buf.Reset()
	root = NewRootCmd(&Options{Name: "kaironctl", Version: "v9.9.9"})
	root.SetOut(&buf)
	root.SetArgs([]string{"version", "--server", "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var got versionInfo
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if got.Client != "v9.9.9" || got.Server == nil || got.Server.Chart != "kairon-0.8.0" || got.Server.Revision != 4 ||
		got.Server.Images["controller"] != "ghcr.io/zyvorai/kairon-controller:0.8.0" {
		t.Fatalf("unexpected version info: %+v / %+v", got, got.Server)
	}
}

func TestStatusStructuredAndExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/deployments/kairon-controller"):
			_, _ = io.WriteString(w, `{"status":{"replicas":1,"readyReplicas":0}}`)
		case strings.HasSuffix(r.URL.Path, "/daemonsets/kairon-node"):
			_, _ = io.WriteString(w, `{"status":{"desiredNumberScheduled":2,"numberReady":2}}`)
		case strings.HasSuffix(r.URL.Path, "/machines"):
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	t.Setenv("KAIRON_KUBE_TOKEN", "")
	oldRel := getReleaseFn
	defer func() { getReleaseFn = oldRel }()
	getReleaseFn = func(*releaseRef) (*release.Release, error) { return nil, errors.New("no release") }

	var execErr error
	out := captureStdout(t, func() {
		root := NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
		root.SetArgs([]string{"status", "-o", "json"})
		execErr = root.Execute()
	})
	if execErr == nil || !strings.Contains(execErr.Error(), "not ready") {
		t.Fatalf("not-ready status must exit non-zero, got %v", execErr)
	}
	var st clusterStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.Ready || st.Controller.Ready != 0 || st.Node.Ready != 2 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestOCIChartResolvesThroughPuller(t *testing.T) {
	want := writeTestChart(t, "0.8.0")
	var gotRef, gotVer string
	old := pullOCIChartFn
	defer func() { pullOCIChartFn = old }()
	pullOCIChartFn = func(ref, ver string) (string, string, error) {
		gotRef, gotVer = ref, ver
		return want, "", nil
	}
	path, tmp, err := resolveChartPathVersion("oci://ghcr.io/zyvorai/charts/kairon", "0.8.0")
	if err != nil || path != want || tmp != "" {
		t.Fatalf("got %q %q %v", path, tmp, err)
	}
	if gotRef != "oci://ghcr.io/zyvorai/charts/kairon" || gotVer != "0.8.0" {
		t.Fatalf("puller called with %q %q", gotRef, gotVer)
	}
	// A filesystem path never touches the registry.
	pullOCIChartFn = func(string, string) (string, string, error) {
		t.Fatal("registry used for a local chart")
		return "", "", nil
	}
	if _, _, err := resolveChartPathVersion(want, "9.9.9"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigViewAndGet(t *testing.T) {
	old := releaseValuesFn
	defer func() { releaseValuesFn = old }()
	releaseValuesFn = func(_ *releaseRef, all bool) (map[string]any, error) {
		if all {
			return map[string]any{"ui": map[string]any{"enabled": true, "service": map[string]any{"port": 18082}}}, nil
		}
		return map[string]any{"ui": map[string]any{"enabled": true}}, nil
	}
	run := func(args ...string) string {
		var buf bytes.Buffer
		root := NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
		root.SetOut(&buf)
		root.SetArgs(append([]string{"config"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return buf.String()
	}
	if got := run("view"); got != "ui:\n  enabled: true\n" {
		t.Fatalf("view: %q", got)
	}
	if got := run("get", "ui.enabled"); got != "true\n" {
		t.Fatalf("get scalar: %q", got)
	}
	if got := run("get", "ui.service.port"); got != "18082\n" {
		t.Fatalf("get computed default: %q", got)
	}
	root := NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"config", "get", "nope.missing"})
	if err := root.Execute(); err == nil {
		t.Fatal("missing key must be an error")
	}
	root = NewRootCmd(&Options{Name: "kaironctl", Version: "v"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"config", "set", "novalue"})
	if err := root.Execute(); err == nil {
		t.Fatal("KEY without =VALUE must be rejected")
	}
}
