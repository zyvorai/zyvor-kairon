// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/release"

	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildHelmUpgradeArgsInstall(t *testing.T) {
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Chart:       "./charts/kairon",
		CreateNS:    true,
		Wait:        true,
		Timeout:     5 * time.Minute,
		Sets:        []string{"ui.enabled=true"},
		ValuesFiles: []string{"extra.yaml"},
		Version:     "0.9.0",
	}
	args := buildHelmUpgradeArgs(h, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"upgrade", "--install", "kairon", "./charts/kairon",
		"--namespace", "kairon-system", "--create-namespace",
		"--version", "0.9.0", "--set", "ui.enabled=true",
		"-f", "extra.yaml", "--wait", "--timeout",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
}

func TestBuildHelmUpgradeArgsUpgradeOnly(t *testing.T) {
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Chart: "./charts/kairon"}
	args := buildHelmUpgradeArgs(h, true)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--install") || strings.Contains(joined, "--create-namespace") {
		t.Fatalf("upgrade-only args should omit --install/--create-namespace: %v", args)
	}
	if args[0] != "upgrade" || args[1] != "kairon" {
		t.Fatalf("args = %v", args)
	}
}

func TestBuildHelmUninstallArgs(t *testing.T) {
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Wait: true, Timeout: time.Minute, DryRun: true}
	args := buildHelmUninstallArgs(h)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "uninstall kairon") && (args[0] != "uninstall" || args[1] != "kairon") {
		t.Fatalf("args = %v", args)
	}
	if !strings.Contains(joined, "--dry-run") || !strings.Contains(joined, "--wait") {
		t.Fatalf("args = %v", args)
	}
}

func TestResolveChartEmbedded(t *testing.T) {
	path, tmp, err := resolveChartPath(embeddedChartSentinel)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if tmp == "" {
		t.Fatal("expected temp dir for embedded chart")
	}
	if _, err := os.Stat(filepath.Join(path, "Chart.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallSDKDryRun(t *testing.T) {
	old := sdkInstallFn
	defer func() { sdkInstallFn = old }()
	called := false
	sdkInstallFn = func(h *helmInstallOpts, upgradeOnly bool) (*release.Release, error) {
		called = true
		if h.Chart != embeddedChartSentinel {
			t.Errorf("chart = %q, want embedded", h.Chart)
		}
		if !h.DryRun {
			t.Error("want DryRun")
		}
		return &release.Release{Manifest: "kind: Namespace\n"}, nil
	}
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Chart:       embeddedChartSentinel,
		DryRun:      true,
		CreateNS:    true,
	}
	if err := runHelmInstall(context.Background(), h, false); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected sdkInstallFn")
	}
}

func TestInstallHelmCLIDryRun(t *testing.T) {
	old := helmRunner
	defer func() { helmRunner = old }()
	called := false
	helmRunner = func(ctx context.Context, args []string) error {
		called = true
		return nil
	}
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Chart:       embeddedChartSentinel,
		DryRun:      true,
		CreateNS:    true,
		HelmCLI:     true,
	}
	if err := runHelmInstall(context.Background(), h, false); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry-run --helm-cli must not invoke helmRunner")
	}
}

func TestRootHelpContainsExamples(t *testing.T) {
	opts := &Options{Name: "kaironctl", Version: "test", Namespace: "default", Color: "never"}
	root := NewRootCmd(opts)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"install", "status", "get machines", "Examples:"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n%s", want, out)
		}
	}
}

func TestVersionNoKube(t *testing.T) {
	code := RunAs("kaironctl", []string{"version"}, "v-test")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestWithDefaultNamespace(t *testing.T) {
	got := withDefaultNamespace("staging", []string{"machines"})
	if len(got) < 3 || got[0] != "--namespace" || got[1] != "staging" || got[2] != "machines" {
		t.Fatalf("got %v", got)
	}
	got = withDefaultNamespace("staging", []string{"-n", "other", "machines"})
	if got[0] != "-n" || got[1] != "other" {
		t.Fatalf("should keep existing -n: %v", got)
	}
	got = withDefaultNamespace("default", []string{"lab-demo", "--image", "/x"})
	if len(got) != 3 || got[0] != "lab-demo" {
		t.Fatalf("default ns must not inject: %v", got)
	}
}

func TestUninstallRefusesWhenMachinesExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines" {
			_ = json.NewEncoder(w).Encode(model.MachineList{
				Items: []model.Machine{{Metadata: model.ObjectMeta{Name: "demo"}}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	t.Setenv("KAIRON_KUBE_TOKEN", "")
	t.Setenv("KAIRON_KUBE_INSECURE", "true")

	old := sdkUninstallFn
	defer func() { sdkUninstallFn = old }()
	sdkUninstallFn = func(h *helmInstallOpts) error {
		t.Fatal("sdk uninstall must not run when Machines exist without --force")
		return nil
	}
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system"}
	err := runHelmUninstall(context.Background(), h)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want refuse-without-force error, got %v", err)
	}
}

func TestUninstallForceSkipsMachineGate(t *testing.T) {
	old := sdkUninstallFn
	defer func() { sdkUninstallFn = old }()
	called := false
	sdkUninstallFn = func(h *helmInstallOpts) error {
		called = true
		if !h.Force {
			t.Error("want Force")
		}
		return nil
	}
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Force: true}
	if err := runHelmUninstall(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected sdkUninstallFn")
	}
}

func TestRemoveCloneHelper(t *testing.T) {
	// cloneHelmOpts kept for callers; smoke that it copies.
	h := &helmInstallOpts{ReleaseName: "x", Chart: "embedded"}
	cp := cloneHelmOpts(h)
	cp.Chart = "other"
	if h.Chart != "embedded" {
		t.Fatal("clone must not mutate original")
	}
}
