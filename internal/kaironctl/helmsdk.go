// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/zyvorai/kairon/charts"
)

const embeddedChartSentinel = "embedded"

// helmDriver is overridden in tests (secrets | configmap | memory).
var helmDriver = "secret"

// newActionConfigFn builds the Helm action configuration; tests replace it
// with an in-memory release store and a fake kube client.
var newActionConfigFn = newActionConfig

type restClientGetter struct {
	namespace string
	cfg       *rest.Config
}

func (g *restClientGetter) ToRESTConfig() (*rest.Config, error) { return g.cfg, nil }
func (g *restClientGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	c, err := discovery.NewDiscoveryClientForConfig(g.cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(c), nil
}
func (g *restClientGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}
func (g *restClientGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return clientcmd.NewDefaultClientConfig(clientcmdapi.Config{}, &clientcmd.ConfigOverrides{
		Context: clientcmdapi.Context{Namespace: g.namespace},
	})
}

func restConfigFromEnv() (*rest.Config, error) {
	explicit := explicitKubeTarget()
	if base := os.Getenv("KAIRON_KUBE_URL"); base != "" && !explicit {
		cfg := &rest.Config{
			Host:        base,
			BearerToken: os.Getenv("KAIRON_KUBE_TOKEN"),
			Burst:       100,
			QPS:         50,
		}
		if os.Getenv("KAIRON_KUBE_INSECURE") == "true" {
			cfg.Insecure = true
		} else if ca := os.Getenv("KAIRON_KUBE_CA"); ca != "" {
			cfg.CAFile = ca
		}
		return cfg, nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" && !explicit {
		return rest.InClusterConfig()
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeFlags.Kubeconfig != "" {
		loadingRules.ExplicitPath = kubeFlags.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeFlags.Context}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func newActionConfig(namespace string) (*action.Configuration, *cli.EnvSettings, error) {
	settings := cli.New()
	cfg, err := restConfigFromEnv()
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes config: %w", err)
	}
	actionConfig := new(action.Configuration)
	getter := &restClientGetter{namespace: namespace, cfg: cfg}
	if err := actionConfig.Init(getter, namespace, helmDriver, func(format string, v ...any) {}); err != nil {
		return nil, nil, fmt.Errorf("helm action init: %w", err)
	}
	return actionConfig, settings, nil
}

// resolveChartPath returns a filesystem chart directory. Empty / "embedded"
// extracts the chart baked into the binary; otherwise uses the given path
// (or KAIRON_CHART). Caller must remove tmpDir when non-empty.
func resolveChartPath(chart string) (path string, tmpDir string, err error) {
	return resolveChartPathVersion(chart, "")
}

// pullOCIChartFn downloads an oci:// chart reference into a temp directory and
// returns the unpacked chart path; replaced in tests (it needs a registry).
var pullOCIChartFn = pullOCIChart

func pullOCIChart(ref, version string) (path string, tmpDir string, err error) {
	rc, err := registry.NewClient()
	if err != nil {
		return "", "", fmt.Errorf("registry client: %w", err)
	}
	cfg := new(action.Configuration)
	cfg.RegistryClient = rc
	tmpDir, err = os.MkdirTemp("", "kairon-chart-oci-*")
	if err != nil {
		return "", "", err
	}
	p := action.NewPullWithOpts(action.WithConfig(cfg))
	p.Settings = cli.New()
	p.Version = version
	p.Untar = true
	p.UntarDir = tmpDir
	p.DestDir = tmpDir
	if _, err := p.Run(ref); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("pull %s: %w", ref, err)
	}
	// The chart unpacks into <tmp>/<chart name>.
	name := ref[strings.LastIndex(ref, "/")+1:]
	if i := strings.IndexAny(name, ":@"); i >= 0 {
		name = name[:i]
	}
	path = filepath.Join(tmpDir, name)
	if _, err := os.Stat(filepath.Join(path, "Chart.yaml")); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", fmt.Errorf("pull %s: unpacked chart not found at %s", ref, path)
	}
	return path, tmpDir, nil
}

// resolveChartPathVersion is resolveChartPath with the chart version used when
// chart is an oci:// reference (for example oci://ghcr.io/zyvorai/charts/kairon).
func resolveChartPathVersion(chart, version string) (path string, tmpDir string, err error) {
	if strings.HasPrefix(chart, "oci://") {
		return pullOCIChartFn(chart, version)
	}
	if chart == "" || chart == embeddedChartSentinel {
		tmpDir, err = os.MkdirTemp("", "kairon-chart-*")
		if err != nil {
			return "", "", err
		}
		path, err = charts.WriteToDir(tmpDir)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", "", err
		}
		return path, tmpDir, nil
	}
	if abs, e := filepath.Abs(chart); e == nil {
		chart = abs
	}
	if _, err := os.Stat(filepath.Join(chart, "Chart.yaml")); err != nil {
		return "", "", fmt.Errorf("chart %q: %w (use --chart embedded for the baked-in chart)", chart, err)
	}
	return chart, "", nil
}

func mergeValues(sets, files []string, settings *cli.EnvSettings) (map[string]any, error) {
	providers := getter.All(settings)
	opts := &values.Options{ValueFiles: files, Values: sets}
	return opts.MergeValues(providers)
}

func sdkInstall(h *helmInstallOpts, upgradeOnly bool) (*release.Release, error) {
	chartPath, tmp, err := resolveChartPathVersion(h.Chart, h.Version)
	if err != nil {
		return nil, err
	}
	if tmp != "" {
		defer func() { _ = os.RemoveAll(tmp) }()
	}
	h, err = withProfile(h, chartPath)
	if err != nil {
		return nil, err
	}

	if h.DryRun {
		manifest, err := renderChartDryRun(chartPath, h)
		if err != nil {
			return nil, err
		}
		return &release.Release{Name: h.ReleaseName, Namespace: h.Namespace, Manifest: manifest}, nil
	}

	actionConfig, settings, err := newActionConfigFn(h.Namespace)
	if err != nil {
		return nil, err
	}
	vals, err := mergeValues(h.Sets, h.ValuesFiles, settings)
	if err != nil {
		return nil, fmt.Errorf("merge values: %w", err)
	}
	ch, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}

	if upgradeOnly {
		u := action.NewUpgrade(actionConfig)
		u.Namespace = h.Namespace
		u.Wait = h.Wait
		u.Timeout = h.Timeout
		u.Atomic = false
		if h.Version != "" {
			u.Version = h.Version
		}
		applyValueReuse(u, h, true)
		if err := applyChartCRDs(actionConfig, ch); err != nil {
			return nil, err
		}
		return u.Run(h.ReleaseName, ch, vals)
	}

	// upgrade --install semantics
	hist := action.NewHistory(actionConfig)
	hist.Max = 1
	_, histErr := hist.Run(h.ReleaseName)
	if histErr == nil {
		u := action.NewUpgrade(actionConfig)
		u.Namespace = h.Namespace
		u.Wait = h.Wait
		u.Timeout = h.Timeout
		if h.Version != "" {
			u.Version = h.Version
		}
		applyValueReuse(u, h, false)
		if err := applyChartCRDs(actionConfig, ch); err != nil {
			return nil, err
		}
		return u.Run(h.ReleaseName, ch, vals)
	}

	inst := action.NewInstall(actionConfig)
	inst.ReleaseName = h.ReleaseName
	inst.Namespace = h.Namespace
	inst.CreateNamespace = h.CreateNS
	inst.Wait = h.Wait
	inst.Timeout = h.Timeout
	if h.Version != "" {
		inst.Version = h.Version
	}
	return inst.Run(ch, vals)
}

// valueReuse reports which Helm value-reuse mode an upgrade should use.
//
// `kaironctl upgrade` keeps the values the release was last installed with
// (Helm's reset-then-reuse: chart defaults of the new chart, then the
// previous user-supplied values, then this call's --set/-f), so an upgrade
// without flags no longer silently resets overrides. --reset-values restores
// the old behaviour and --reuse-values reuses the previous computed values
// verbatim. `install` re-runs keep Helm's reset semantics.
func valueReuse(h *helmInstallOpts, upgradeOnly bool) (reuse, resetThenReuse bool) {
	switch {
	case h.ResetValues:
		return false, false
	case h.ReuseValues:
		return true, false
	case upgradeOnly:
		return false, true
	}
	return false, false
}

func applyValueReuse(u *action.Upgrade, h *helmInstallOpts, upgradeOnly bool) {
	u.ReuseValues, u.ResetThenReuseValues = valueReuse(h, upgradeOnly)
}

// withProfile prepends the chart's values-<profile>.yaml (for example
// values-production.yaml) to the values files so that -f and --set override it.
func withProfile(h *helmInstallOpts, chartPath string) (*helmInstallOpts, error) {
	if h.Profile == "" || h.Profile == "evaluation" || h.Profile == "default" {
		return h, nil
	}
	if strings.ContainsAny(h.Profile, "/\\.") {
		return nil, fmt.Errorf("invalid --profile %q", h.Profile)
	}
	f := filepath.Join(chartPath, "values-"+h.Profile+".yaml")
	if _, err := os.Stat(f); err != nil {
		return nil, fmt.Errorf("--profile %q: %s not found in chart (profiles: evaluation, production)", h.Profile, filepath.Base(f))
	}
	cp := *h
	cp.ValuesFiles = append([]string{f}, h.ValuesFiles...)
	return &cp, nil
}

func renderChartDryRun(chartPath string, h *helmInstallOpts) (string, error) {
	settings := cli.New()
	vals, err := mergeValues(h.Sets, h.ValuesFiles, settings)
	if err != nil {
		return "", fmt.Errorf("merge values: %w", err)
	}
	ch, err := loader.Load(chartPath)
	if err != nil {
		return "", fmt.Errorf("load chart: %w", err)
	}
	options := chartutil.ReleaseOptions{
		Name:      h.ReleaseName,
		Namespace: h.Namespace,
		IsInstall: true,
	}
	values, err := chartutil.ToRenderValues(ch, vals, options, chartutil.DefaultCapabilities)
	if err != nil {
		return "", err
	}
	files, err := engine.Render(ch, values)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for name, content := range files {
		if strings.HasSuffix(name, "NOTES.txt") || strings.TrimSpace(content) == "" {
			continue
		}
		b.WriteString("---\n# Source: ")
		b.WriteString(name)
		b.WriteString("\n")
		b.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), nil
}

func sdkUninstall(h *helmInstallOpts) error {
	actionConfig, _, err := newActionConfig(h.Namespace)
	if err != nil {
		return err
	}
	un := action.NewUninstall(actionConfig)
	un.Wait = h.Wait
	un.Timeout = h.Timeout
	un.DryRun = h.DryRun
	_, err = un.Run(h.ReleaseName)
	return err
}

// applyChartCRDs creates or updates the chart's crds/ directory before an
// upgrade. Helm only installs crds/ on first install and never touches them
// on upgrade, so without this a newer kaironctl rolls out new binaries
// against the old CRDs (docs/guides/crd-versioning.md). Existing CRDs are
// patched with a three-way merge, new ones are created.
func applyChartCRDs(cfg *action.Configuration, ch *chart.Chart) error {
	crds := ch.CRDObjects()
	if len(crds) == 0 {
		return nil
	}
	var data bytes.Buffer
	for _, c := range crds {
		data.Write(c.File.Data)
		data.WriteString("\n---\n")
	}
	original, err := cfg.KubeClient.Build(bytes.NewReader(data.Bytes()), false)
	if err != nil {
		return fmt.Errorf("build chart CRDs: %w", err)
	}
	target, err := cfg.KubeClient.Build(bytes.NewReader(data.Bytes()), false)
	if err != nil {
		return fmt.Errorf("build chart CRDs: %w", err)
	}
	if _, err := cfg.KubeClient.Update(original, target, false); err != nil {
		return fmt.Errorf("apply chart CRDs before upgrade: %w", err)
	}
	return nil
}
