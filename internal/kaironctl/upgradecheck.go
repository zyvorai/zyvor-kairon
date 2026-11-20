// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
)

// latestReleaseURL is the GitHub release feed; KAIRON_RELEASE_API overrides it
// (mirrors, air-gapped lookups, tests).
const latestReleaseURL = "https://api.github.com/repos/zyvorai/kairon/releases/latest"

// parseSemver splits "v1.2.3" / "1.2.3-rc1" into numeric parts; ok is false
// for anything that is not major.minor.patch.
func parseSemver(v string) (parts [3]int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	f := strings.Split(v, ".")
	if len(f) != 3 {
		return parts, false
	}
	for i, s := range f {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// compareSemver returns -1, 0 or 1; ok is false when either side is not semver.
func compareSemver(a, b string) (int, bool) {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

// minorBump reports whether moving from a to b changes major or minor.
func minorBump(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	return oka && okb && (pa[0] != pb[0] || pa[1] != pb[1])
}

func fetchLatestRelease(ctx context.Context) (string, error) {
	url := os.Getenv("KAIRON_RELEASE_API")
	if url == "" {
		url = latestReleaseURL
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no tag_name in response")
	}
	return rel.TagName, nil
}

// runUpgradeCheck compares the installed chart, the chart this kaironctl
// would apply, and the latest published release. It changes nothing.
func runUpgradeCheck(ctx context.Context, h *helmInstallOpts, out io.Writer) error {
	installed := ""
	if rel, err := getReleaseFn(&releaseRef{Name: h.ReleaseName, Namespace: h.Namespace}); err == nil && rel != nil && rel.Chart != nil && rel.Chart.Metadata != nil {
		installed = rel.Chart.Metadata.Version
	} else if err != nil {
		return fmt.Errorf("read release %s/%s: %w", h.Namespace, h.ReleaseName, err)
	}
	path, tmp, err := resolveChartPathVersion(h.Chart, h.Version)
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
	available := ch.Metadata.Version
	latest, latestErr := fetchLatestRelease(ctx)

	_, _ = fmt.Fprintf(out, "Installed chart:      %s\n", orDash(installed))
	_, _ = fmt.Fprintf(out, "Chart to apply:       %s (%s)\n", available, chartSource(h.Chart))
	if latestErr != nil {
		_, _ = fmt.Fprintf(out, "Latest release:       unavailable (%v)\n", latestErr)
	} else {
		_, _ = fmt.Fprintf(out, "Latest release:       %s\n", latest)
	}

	target := available
	if latestErr == nil {
		if c, ok := compareSemver(latest, available); ok && c > 0 {
			_, _ = fmt.Fprintf(out, "\nA newer release (%s) is published than the chart in this kaironctl.\nGet the matching kaironctl, or: kaironctl upgrade --chart oci://ghcr.io/zyvorai/charts/kairon --version %s\n",
				latest, strings.TrimPrefix(latest, "v"))
			target = strings.TrimPrefix(latest, "v")
		}
	}
	c, ok := compareSemver(installed, target)
	switch {
	case installed == "":
		_, _ = fmt.Fprintln(out, "\nNo installed release found; use `kaironctl install`.")
	case ok && c == 0:
		_, _ = fmt.Fprintln(out, "\nUp to date.")
	case ok && c > 0:
		_, _ = fmt.Fprintf(out, "\nThe installed chart (%s) is newer than the one this kaironctl would apply (%s); use a newer kaironctl before upgrading.\n", installed, target)
	default:
		_, _ = fmt.Fprintf(out, "\nUpgrade available: %s -> %s.\n", installed, target)
		if minorBump(installed, target) {
			_, _ = fmt.Fprintln(out, "This crosses a minor version: apply the CRDs from the target release first (Helm does not upgrade crds/):")
			_, _ = fmt.Fprintf(out, "  kubectl apply --server-side -f https://raw.githubusercontent.com/zyvorai/kairon/v%s/deploy/crd.yaml\n", strings.TrimPrefix(target, "v"))
		}
		_, _ = fmt.Fprintln(out, "Preview with: kaironctl upgrade --dry-run; apply with: kaironctl upgrade --wait")
	}
	return nil
}

func chartSource(chart string) string {
	if chart == "" || chart == embeddedChartSentinel {
		return "embedded in this kaironctl"
	}
	return chart
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
