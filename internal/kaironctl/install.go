// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

// helmRunner is the CLI fallback, overridden in tests.
var helmRunner = func(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// sdkInstallFn / sdkUninstallFn are overridden in tests.
var sdkInstallFn = sdkInstall
var sdkUninstallFn = sdkUninstall

type helmInstallOpts struct {
	ReleaseName string
	Namespace   string
	Chart       string
	Version     string
	Sets        []string
	ValuesFiles []string
	Wait        bool
	Timeout     time.Duration
	DryRun      bool
	CreateNS    bool
	Force       bool // uninstall only
	HelmCLI     bool // shell out to helm instead of the embedded SDK

	// Profile selects a values-<profile>.yaml shipped in the chart.
	Profile string
	// ResetValues / ReuseValues control how an upgrade treats the previous
	// release's values; see valueReuse.
	ResetValues bool
	ReuseValues bool
}

func defaultChartPath() string {
	if v := os.Getenv("KAIRON_CHART"); v != "" {
		return v
	}
	return embeddedChartSentinel
}

func newInstallCmd(opts *Options) *cobra.Command {
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Chart:       defaultChartPath(),
		Timeout:     5 * time.Minute,
		CreateNS:    true,
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Kairon into a Kubernetes cluster using Helm",
		Long: `Install Kairon (controller, node agent, CRDs) via the embedded Helm SDK.

The chart is baked into kaironctl (no local ./charts/kairon or helm binary required).
Pass --chart PATH to use a checkout, or --helm-cli to shell out to helm on PATH.`,
		Example: `  $ kaironctl install
  $ kaironctl install --set ui.enabled=true --wait
  $ kaironctl install --chart ./charts/kairon --helm-cli
  $ kaironctl install --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHelmInstall(cmd.Context(), h, false)
		},
	}
	bindHelmFlags(cmd, h, true)
	return cmd
}

func newUpgradeCmd(opts *Options) *cobra.Command {
	var check bool
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Chart:       defaultChartPath(),
		Timeout:     5 * time.Minute,
	}
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade an existing Kairon Helm release",
		Long:  `Upgrade the Kairon Helm release in-place (embedded Helm SDK, or --helm-cli).`,
		Example: `  $ kaironctl upgrade --set controller.replicaCount=2 --wait
  $ kaironctl upgrade --chart ./charts/kairon`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check {
				ctx := cmd.Context()
				if ctx == nil {
					ctx = context.Background()
				}
				return runUpgradeCheck(ctx, h, cmd.OutOrStdout())
			}
			return runHelmInstall(cmd.Context(), h, true)
		},
	}
	bindHelmFlags(cmd, h, false)
	cmd.Flags().BoolVar(&check, "check", false, "compare the installed chart with this kaironctl's chart and the latest release; change nothing")
	return cmd
}

func newUninstallCmd(opts *Options) *cobra.Command {
	h := &helmInstallOpts{
		ReleaseName: "kairon",
		Namespace:   "kairon-system",
		Timeout:     5 * time.Minute,
	}
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall Kairon from a Kubernetes cluster",
		Long: `Uninstall the Kairon Helm release (embedded Helm SDK, or --helm-cli).

Refuses to proceed while any Machine objects still exist unless --force is set.`,
		Example: `  $ kaironctl uninstall
  $ kaironctl uninstall --force --wait`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHelmUninstall(cmd.Context(), h)
		},
	}
	cmd.Flags().StringVar(&h.ReleaseName, "helm-release-name", h.ReleaseName, "Helm release name")
	cmd.Flags().StringVarP(&h.Namespace, "namespace", "n", h.Namespace, "Helm release namespace")
	cmd.Flags().BoolVar(&h.Wait, "wait", false, "wait for resources to be deleted")
	cmd.Flags().DurationVar(&h.Timeout, "timeout", h.Timeout, "time to wait for uninstall")
	cmd.Flags().BoolVar(&h.DryRun, "dry-run", false, "simulate uninstall without deleting")
	cmd.Flags().BoolVar(&h.Force, "force", false, "uninstall even if Machine objects still exist")
	cmd.Flags().BoolVar(&h.HelmCLI, "helm-cli", false, "shell out to helm on PATH instead of the embedded SDK")
	return cmd
}

func bindHelmFlags(cmd *cobra.Command, h *helmInstallOpts, createNS bool) {
	cmd.Flags().StringVar(&h.ReleaseName, "helm-release-name", h.ReleaseName, "Helm release name")
	cmd.Flags().StringVarP(&h.Namespace, "namespace", "n", h.Namespace, "install namespace")
	cmd.Flags().StringVar(&h.Chart, "chart", h.Chart, "chart path, or \"embedded\" for the baked-in chart (or KAIRON_CHART)")
	cmd.Flags().StringVar(&h.Version, "version", "", "chart version (OCI/repo charts; ignored for embedded filesystem charts)")
	cmd.Flags().StringArrayVar(&h.Sets, "set", nil, "helm --set key=value (repeatable)")
	cmd.Flags().StringArrayVarP(&h.ValuesFiles, "values", "f", nil, "helm values file (repeatable)")
	cmd.Flags().BoolVar(&h.Wait, "wait", false, "wait until resources are ready")
	cmd.Flags().DurationVar(&h.Timeout, "timeout", h.Timeout, "time to wait for --wait")
	cmd.Flags().BoolVar(&h.DryRun, "dry-run", false, "render/simulate without applying; mute progress chatter")
	cmd.Flags().BoolVar(&h.HelmCLI, "helm-cli", false, "shell out to helm on PATH instead of the embedded SDK")
	cmd.Flags().StringVar(&h.Profile, "profile", "", "values profile shipped in the chart: evaluation (default) or production")
	if createNS {
		cmd.Flags().BoolVar(&h.CreateNS, "create-namespace", true, "create namespace if missing")
	} else {
		cmd.Flags().BoolVar(&h.ResetValues, "reset-values", false, "discard the previous release's values and use chart defaults plus --set/-f")
		cmd.Flags().BoolVar(&h.ReuseValues, "reuse-values", false, "reuse the previous release's computed values verbatim (chart default changes are not picked up)")
	}
}

func buildHelmUpgradeArgs(h *helmInstallOpts, upgradeOnly bool) []string {
	chart := h.Chart
	if chart == "" || chart == embeddedChartSentinel {
		chart = "./charts/kairon"
	}
	args := []string{"upgrade", "--install", h.ReleaseName, chart, "--namespace", h.Namespace}
	if upgradeOnly {
		args = []string{"upgrade", h.ReleaseName, chart, "--namespace", h.Namespace}
	}
	if h.CreateNS && !upgradeOnly {
		args = append(args, "--create-namespace")
	}
	if h.Version != "" {
		args = append(args, "--version", h.Version)
	}
	reuse, thenReuse := valueReuse(h, upgradeOnly)
	if reuse {
		args = append(args, "--reuse-values")
	}
	if thenReuse {
		args = append(args, "--reset-then-reuse-values")
	}
	for _, s := range h.Sets {
		args = append(args, "--set", s)
	}
	for _, f := range h.ValuesFiles {
		args = append(args, "-f", f)
	}
	if h.Wait {
		args = append(args, "--wait", "--timeout", formatHelmTimeout(h.Timeout))
	}
	if h.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

func buildHelmUninstallArgs(h *helmInstallOpts) []string {
	args := []string{"uninstall", h.ReleaseName, "--namespace", h.Namespace}
	if h.Wait {
		args = append(args, "--wait", "--timeout", formatHelmTimeout(h.Timeout))
	}
	if h.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

func formatHelmTimeout(d time.Duration) string {
	if d < time.Second {
		return "1s"
	}
	return d.String()
}

func runHelmInstall(ctx context.Context, h *helmInstallOpts, upgradeOnly bool) error {
	style.SetQuiet(h.DryRun)
	chartLabel := h.Chart
	if chartLabel == "" || chartLabel == embeddedChartSentinel {
		style.Log(style.EmojiDetect, "Using embedded Helm chart")
	} else if abs, err := filepath.Abs(chartLabel); err == nil && fileExists(chartLabel) {
		style.Log(style.EmojiDetect, "Using chart path %s", abs)
	} else {
		style.Log(style.EmojiInfo, "Using chart %s", chartLabel)
	}
	style.Log(style.EmojiInfo, "Release %s in namespace %s", h.ReleaseName, h.Namespace)

	if h.HelmCLI {
		return runHelmCLIInstall(ctx, h, upgradeOnly)
	}

	style.Log(style.EmojiRocket, "Installing via embedded Helm SDK…")
	rel, err := sdkInstallFn(h, upgradeOnly)
	if err != nil {
		return fmt.Errorf("helm sdk: %w", err)
	}
	action := "install"
	if upgradeOnly {
		action = "upgrade"
	}
	if h.DryRun && rel != nil {
		fmt.Print(rel.Manifest)
	}
	style.Log(style.EmojiOK, "Kairon %s complete", action)
	selftestHint()
	return nil
}

func runHelmCLIInstall(ctx context.Context, h *helmInstallOpts, upgradeOnly bool) error {
	chartPath, tmp, err := resolveChartPathVersion(h.Chart, h.Version)
	if err != nil {
		return err
	}
	if tmp != "" {
		defer func() { _ = os.RemoveAll(tmp) }()
	}
	cp := *h
	cp.Chart = chartPath
	h = &cp
	if h, err = withProfile(h, chartPath); err != nil {
		return err
	}
	if _, err := exec.LookPath("helm"); err != nil {
		style.Failf("helm not found on PATH (needed for --helm-cli)")
		return fmt.Errorf("helm not found on PATH")
	}
	args := buildHelmUpgradeArgs(h, upgradeOnly)
	if h.DryRun {
		fmt.Println("helm", strings.Join(args, " "))
		return nil
	}
	style.Log(style.EmojiRocket, "Running helm %s…", args[0])
	if err := helmRunner(ctx, args); err != nil {
		return fmt.Errorf("helm %s: %w", args[0], err)
	}
	action := "install"
	if upgradeOnly {
		action = "upgrade"
	}
	style.Log(style.EmojiOK, "Kairon %s complete", action)
	selftestHint()
	return nil
}

func cloneHelmOpts(h *helmInstallOpts) *helmInstallOpts {
	cp := *h
	return &cp
}

func runHelmUninstall(ctx context.Context, h *helmInstallOpts) error {
	style.SetQuiet(h.DryRun)
	if !h.Force && !h.DryRun {
		kc, err := newKubeClient()
		if err != nil {
			return fmt.Errorf("connect to cluster to check for Machines (or pass --force): %w", err)
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		machines, err := kc.ListMachines(cctx)
		if err != nil {
			return fmt.Errorf("list Machines before uninstall: %w", err)
		}
		if len(machines) > 0 {
			style.Failf("%d Machine(s) still exist — delete them first, or pass --force", len(machines))
			return fmt.Errorf("%d Machine(s) still exist; refuse uninstall without --force", len(machines))
		}
		style.Log(style.EmojiOK, "No Machines found; safe to uninstall")
	} else if h.Force {
		style.Log(style.EmojiWarn, "Forcing uninstall (--force); Machines may be left orphaned")
	}

	style.Log(style.EmojiFire, "Uninstalling release %s from %s", h.ReleaseName, h.Namespace)
	if h.HelmCLI {
		if _, err := exec.LookPath("helm"); err != nil && !h.DryRun {
			style.Failf("helm not found on PATH")
			return fmt.Errorf("helm not found on PATH")
		}
		args := buildHelmUninstallArgs(h)
		if h.DryRun {
			fmt.Println("helm", strings.Join(args, " "))
			return nil
		}
		if err := helmRunner(ctx, args); err != nil {
			return fmt.Errorf("helm uninstall: %w", err)
		}
	} else {
		if err := sdkUninstallFn(h); err != nil {
			return fmt.Errorf("helm sdk uninstall: %w", err)
		}
	}
	style.Log(style.EmojiOK, "Kairon uninstalled")
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// selftestHint tells the operator how to confirm the install really works.
func selftestHint() {
	style.Log(style.EmojiInfo, "Check that it all works (boots a tiny CirrOS VM, then removes it): kaironctl selftest --profile quick")
}
