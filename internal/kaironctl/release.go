// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

// releaseRef names the Helm release the release-management commands act on.
type releaseRef struct {
	Name      string
	Namespace string
}

func newReleaseRef() *releaseRef {
	return &releaseRef{Name: "kairon", Namespace: "kairon-system"}
}

func (r *releaseRef) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&r.Name, "helm-release-name", r.Name, "Helm release name")
	cmd.Flags().StringVarP(&r.Namespace, "namespace", "n", r.Namespace, "Helm release namespace")
}

// getReleaseFn / releaseValuesFn / releaseHistoryFn are the Helm read seams.
var (
	getReleaseFn = func(ref *releaseRef) (*release.Release, error) {
		cfg, _, err := newActionConfigFn(ref.Namespace)
		if err != nil {
			return nil, err
		}
		return action.NewGet(cfg).Run(ref.Name)
	}
	releaseValuesFn = func(ref *releaseRef, all bool) (map[string]any, error) {
		cfg, _, err := newActionConfigFn(ref.Namespace)
		if err != nil {
			return nil, err
		}
		g := action.NewGetValues(cfg)
		g.AllValues = all
		return g.Run(ref.Name)
	}
	releaseHistoryFn = func(ref *releaseRef) ([]*release.Release, error) {
		cfg, _, err := newActionConfigFn(ref.Namespace)
		if err != nil {
			return nil, err
		}
		return action.NewHistory(cfg).Run(ref.Name)
	}
	rollbackFn = func(ref *releaseRef, revision int, wait bool, timeout time.Duration, dryRun bool) error {
		cfg, _, err := newActionConfigFn(ref.Namespace)
		if err != nil {
			return err
		}
		rb := action.NewRollback(cfg)
		rb.Version = revision
		rb.Wait = wait
		rb.Timeout = timeout
		rb.DryRun = dryRun
		return rb.Run(ref.Name)
	}
)

// writeStructured prints v as JSON or YAML.
func writeStructured(w io.Writer, v any, output string) error {
	switch output {
	case "json":
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "yaml", "":
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	return fmt.Errorf("unknown output %q (want yaml or json)", output)
}

func newConfigCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and change the settings of the installed Kairon release",
		Long: `View and change the Helm values of the installed Kairon release.

view and get read the release. set and unset apply an in-place upgrade that keeps
every other value, so a single setting can change without restating the rest.`,
		Example: `  $ kaironctl config view
  $ kaironctl config get ui.enabled
  $ kaironctl config set ui.enabled=true controller.replicaCount=2 --wait
  $ kaironctl config unset ui.atlas.url`,
	}
	cmd.AddCommand(newConfigViewCmd(), newConfigGetCmd(), newConfigSetCmd(), newConfigUnsetCmd())
	return cmd
}

func newConfigViewCmd() *cobra.Command {
	ref := newReleaseRef()
	var all bool
	var output string
	cmd := &cobra.Command{
		Use:   "view",
		Short: "Print the release's user-supplied values (or all values with --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			vals, err := releaseValuesFn(ref, all)
			if err != nil {
				return fmt.Errorf("read release %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			if len(vals) == 0 {
				vals = map[string]any{}
			}
			return writeStructured(cmd.OutOrStdout(), vals, output)
		},
	}
	ref.bind(cmd)
	cmd.Flags().BoolVar(&all, "all", false, "include chart defaults (computed values)")
	cmd.Flags().StringVarP(&output, "output", "o", "yaml", "output format: yaml|json")
	return cmd
}

func newConfigGetCmd() *cobra.Command {
	ref := newReleaseRef()
	var output string
	cmd := &cobra.Command{
		Use:   "get KEY",
		Short: "Print one value by dotted path (for example ui.enabled)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vals, err := releaseValuesFn(ref, true)
			if err != nil {
				return fmt.Errorf("read release %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			v, err := chartutil.Values(vals).PathValue(args[0])
			if err != nil {
				return fmt.Errorf("no value %q in release %s/%s", args[0], ref.Namespace, ref.Name)
			}
			switch v.(type) {
			case map[string]any, []any:
				return writeStructured(cmd.OutOrStdout(), v, output)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), v)
			return err
		},
	}
	ref.bind(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "yaml", "format for map and list values: yaml|json")
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Chart: defaultChartPath(), Timeout: 5 * time.Minute}
	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE...]",
		Short: "Change one or more values of the installed release",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if !strings.Contains(a, "=") || strings.HasPrefix(a, "=") {
					return fmt.Errorf("%q is not KEY=VALUE", a)
				}
			}
			h.Sets = args
			return applyConfigChange(cmd.Context(), h)
		},
	}
	bindConfigChangeFlags(cmd, h)
	return cmd
}

func newConfigUnsetCmd() *cobra.Command {
	h := &helmInstallOpts{ReleaseName: "kairon", Namespace: "kairon-system", Chart: defaultChartPath(), Timeout: 5 * time.Minute}
	cmd := &cobra.Command{
		Use:   "unset KEY [KEY...]",
		Short: "Remove one or more user-supplied values (falls back to the chart default)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if strings.Contains(a, "=") || a == "" {
					return fmt.Errorf("%q is not a bare KEY", a)
				}
				h.Sets = append(h.Sets, a+"=null")
			}
			return applyConfigChange(cmd.Context(), h)
		},
	}
	bindConfigChangeFlags(cmd, h)
	return cmd
}

func bindConfigChangeFlags(cmd *cobra.Command, h *helmInstallOpts) {
	cmd.Flags().StringVar(&h.ReleaseName, "helm-release-name", h.ReleaseName, "Helm release name")
	cmd.Flags().StringVarP(&h.Namespace, "namespace", "n", h.Namespace, "Helm release namespace")
	cmd.Flags().StringVar(&h.Chart, "chart", h.Chart, "chart path, or \"embedded\" for the baked-in chart (or KAIRON_CHART)")
	cmd.Flags().BoolVar(&h.Wait, "wait", false, "wait until resources are ready")
	cmd.Flags().DurationVar(&h.Timeout, "timeout", h.Timeout, "time to wait for --wait")
	cmd.Flags().BoolVar(&h.DryRun, "dry-run", false, "render without applying")
	cmd.Flags().BoolVar(&h.HelmCLI, "helm-cli", false, "shell out to helm on PATH instead of the embedded SDK")
}

// applyConfigChange upgrades the installed release with only h.Sets changed.
// It refuses when the chart that would be applied is not the chart version
// the release runs: a one-value change must never double as a chart upgrade.
func applyConfigChange(ctx context.Context, h *helmInstallOpts) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !h.DryRun {
		rel, err := getReleaseFn(&releaseRef{Name: h.ReleaseName, Namespace: h.Namespace})
		if err != nil {
			return fmt.Errorf("read release %s/%s: %w", h.Namespace, h.ReleaseName, err)
		}
		if err := ensureSameChart(rel, h.Chart); err != nil {
			return err
		}
	}
	return runHelmInstall(ctx, h, true)
}

func ensureSameChart(rel *release.Release, chart string) error {
	if rel == nil || rel.Chart == nil || rel.Chart.Metadata == nil {
		return nil
	}
	path, tmp, err := resolveChartPath(chart)
	if err != nil {
		return err
	}
	if tmp != "" {
		defer func() { _ = os.RemoveAll(tmp) }()
	}
	ch, err := loader.Load(path)
	if err != nil {
		return fmt.Errorf("load chart: %w", err)
	}
	have, want := rel.Chart.Metadata.Version, ch.Metadata.Version
	if have != want {
		return fmt.Errorf("installed chart is %s but this kaironctl would apply chart %s: run `kaironctl upgrade` first, or pass --chart for the installed chart", have, want)
	}
	return nil
}

func newHistoryCmd() *cobra.Command {
	ref := newReleaseRef()
	var output string
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show the revision history of the Kairon release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			revs, err := releaseHistoryFn(ref)
			if err != nil {
				return fmt.Errorf("read release %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			sort.Slice(revs, func(i, j int) bool { return revs[i].Version < revs[j].Version })
			return writeHistory(cmd.OutOrStdout(), revs, output)
		},
	}
	ref.bind(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json|yaml")
	return cmd
}

type historyRow struct {
	Revision    int       `json:"revision"`
	Updated     time.Time `json:"updated"`
	Status      string    `json:"status"`
	Chart       string    `json:"chart"`
	AppVersion  string    `json:"appVersion"`
	Description string    `json:"description"`
}

func historyRows(revs []*release.Release) []historyRow {
	rows := make([]historyRow, 0, len(revs))
	for _, r := range revs {
		row := historyRow{Revision: r.Version}
		if r.Info != nil {
			row.Updated = r.Info.LastDeployed.Time
			row.Status = string(r.Info.Status)
			row.Description = r.Info.Description
		}
		if r.Chart != nil && r.Chart.Metadata != nil {
			row.Chart = r.Chart.Metadata.Name + "-" + r.Chart.Metadata.Version
			row.AppVersion = r.Chart.Metadata.AppVersion
		}
		rows = append(rows, row)
	}
	return rows
}

func writeHistory(w io.Writer, revs []*release.Release, output string) error {
	rows := historyRows(revs)
	if output == "json" || output == "yaml" {
		return writeStructured(w, rows, output)
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REVISION\tUPDATED\tSTATUS\tCHART\tAPP VERSION\tDESCRIPTION")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Revision, r.Updated.Format(time.RFC3339), r.Status, r.Chart, r.AppVersion, r.Description)
	}
	return tw.Flush()
}

func newRollbackCmd() *cobra.Command {
	ref := newReleaseRef()
	var wait, dryRun bool
	timeout := 5 * time.Minute
	cmd := &cobra.Command{
		Use:   "rollback [REVISION]",
		Short: "Roll the Kairon release back to a previous revision (default: the previous one)",
		Long: `Roll the Kairon Helm release back to a previous revision. Rolling back restores the
chart and values of that revision; it does not downgrade CRD versions already applied to the cluster.`,
		Example: `  $ kaironctl history
  $ kaironctl rollback
  $ kaironctl rollback 2 --wait`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rev := 0
			if len(args) == 1 {
				if _, err := fmt.Sscanf(args[0], "%d", &rev); err != nil || rev < 1 {
					return fmt.Errorf("REVISION must be a positive integer, got %q", args[0])
				}
			}
			if err := rollbackFn(ref, rev, wait, timeout, dryRun); err != nil {
				return fmt.Errorf("rollback %s/%s: %w", ref.Namespace, ref.Name, err)
			}
			target := "the previous revision"
			if rev > 0 {
				target = fmt.Sprintf("revision %d", rev)
			}
			style.Log(style.EmojiOK, "Rolled %s back to %s", ref.Name, target)
			return nil
		},
	}
	ref.bind(cmd)
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until resources are ready")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "time to wait for --wait")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "simulate the rollback")
	return cmd
}
