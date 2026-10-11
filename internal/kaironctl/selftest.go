// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/selftest"
)

type selftestOpts struct {
	Profile       string
	Only, Skip    []string
	List          bool
	Cleanup       bool
	Image         string
	ImageURL      string
	ImageDigest   string
	Arch          string
	InstallNS     string
	TestNS        string
	UIURL         string
	UIToken       string
	Bridge        string
	PVC           string
	Keep          bool
	NoVM          bool
	Strict        bool
	LogFile       string
	Timeout       time.Duration
	BootTimeout   time.Duration
	Output        string
	NoEvidence    bool
	EvidenceLines int
}

func newSelftestCmd(opts *Options) *cobra.Command {
	o := &selftestOpts{Profile: "standard", InstallNS: "kairon-system", TestNS: "default", Timeout: 20 * time.Minute, BootTimeout: 4 * time.Minute, EvidenceLines: 400}
	cmd := &cobra.Command{
		Use:   "selftest",
		Short: "Boot a small CirrOS VM and check that Kairon really works",
		Long: `Run a short end-to-end check of a live install. Besides the control-plane checks of
'kaironctl doctor', it creates a tiny CirrOS Machine, waits for the guest to boot, then
exercises power, pause/resume, networking, CPU hotplug and observability, and (with
--profile full, when the cluster allows) fork and cold migration. Every step prints
timestamped, coloured log lines; every Machine it creates is deleted afterwards, even on
Ctrl-C.

Profiles: quick (control plane, boot, console log, power), standard (adds network,
pause/resume, hotplug, observability), full (adds NIC/disk hotplug, fork, migration).
A check whose prerequisite is missing is SKIPPED with the reason, never silently dropped.

The CirrOS image is downloaded by kairon-node from download.cirros-cloud.net and
verified against a pinned sha256. For an air-gapped cluster use --image PATH (a file
already on the node) or --image-url with --image-digest.

Reading the guest's serial console and the dashboard's usage history needs the dashboard
API: set --ui-url and --ui-token (or KAIRON_UI_URL / KAIRON_UI_TOKEN).`,
		Example: `  $ kaironctl selftest                      # standard profile
  $ kaironctl selftest --profile quick      # about a minute
  $ kaironctl selftest --no-vm              # control plane only, creates nothing
  $ kaironctl selftest --image /var/lib/fluxvm/images/cirros.qcow2
  $ kaironctl selftest --list
  $ kaironctl selftest --cleanup            # remove Machines a killed run left behind`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runSelftest(cmd, o, opts.Version) },
	}
	f := cmd.Flags()
	f.StringVar(&o.Profile, "profile", o.Profile, "quick|standard|full")
	f.StringSliceVar(&o.Only, "only", nil, "run only these scenario ids (see --list)")
	f.StringSliceVar(&o.Skip, "skip", nil, "skip these scenario ids")
	f.BoolVar(&o.List, "list", false, "list scenarios and exit")
	f.BoolVar(&o.Cleanup, "cleanup", false, "delete leftover self-test Machines and exit")
	f.StringVar(&o.Image, "image", "", "boot this node-local image path instead of downloading CirrOS")
	f.StringVar(&o.ImageURL, "image-url", "", "download this image URL instead of the pinned CirrOS (needs --image-digest)")
	f.StringVar(&o.ImageDigest, "image-digest", "", "sha256:... of --image-url")
	f.StringVar(&o.Arch, "arch", "", "amd64|arm64 (default: detected from a capable node)")
	f.StringVarP(&o.InstallNS, "namespace", "n", o.InstallNS, "namespace where Kairon is installed")
	f.StringVar(&o.TestNS, "test-namespace", o.TestNS, "namespace for the test Machines")
	f.StringVar(&o.UIURL, "ui-url", "", "kairon-ui base URL (default $KAIRON_UI_URL)")
	f.StringVar(&o.UIToken, "ui-token", "", "kairon-ui bearer token (default $KAIRON_UI_TOKEN)")
	f.StringVar(&o.Bridge, "bridge", "", "host bridge to enable the NIC hotplug check (full)")
	f.StringVar(&o.PVC, "pvc", "", "existing PVC to enable the disk hotplug check (full)")
	f.BoolVar(&o.Keep, "keep", false, "do not delete the test Machines (debugging)")
	f.BoolVar(&o.NoVM, "no-vm", false, "control-plane checks only; creates no Machine")
	f.BoolVar(&o.Strict, "strict", false, "exit non-zero when a check is skipped")
	f.StringVar(&o.LogFile, "log-file", "", "also write the run, without colour, to this file")
	f.DurationVar(&o.Timeout, "timeout", o.Timeout, "overall time limit")
	f.DurationVar(&o.BootTimeout, "boot-timeout", o.BootTimeout, "how long to wait for a guest to boot")
	f.StringVarP(&o.Output, "output", "o", "", "json: print a machine-readable report instead of the log")
	f.BoolVar(&o.NoEvidence, "no-evidence", false, "do not print controller/node log lines after a failure")
	return cmd
}

func runSelftest(cmd *cobra.Command, o *selftestOpts, version string) error {
	out := cmd.OutOrStdout()
	if o.List {
		listSelftest(out)
		return nil
	}
	profile, err := selftest.ParseProfile(o.Profile)
	if err != nil {
		return err
	}
	if bad := selftest.UnknownIDs(append(append([]string{}, o.Only...), o.Skip...)); len(bad) > 0 {
		return fmt.Errorf("unknown scenario(s) %s (see kaironctl selftest --list)", strings.Join(bad, ", "))
	}
	if o.Output != "" && o.Output != "json" {
		return fmt.Errorf("unknown output %q (want json)", o.Output)
	}
	if o.UIURL != "" {
		_ = os.Setenv("KAIRON_UI_URL", o.UIURL)
	}
	if o.UIToken != "" {
		_ = os.Setenv("KAIRON_UI_TOKEN", o.UIToken)
	}

	kc, err := newKubeClient()
	if err != nil {
		return err
	}
	base := cmd.Context()
	if base == nil {
		base = context.Background()
	}
	ctx, stop := signal.NotifyContext(base, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	if o.Cleanup {
		return cleanupSelftest(ctx, kc, out)
	}

	runID := newRunID()
	env := selftest.Env{
		Kube: kc, Namespace: o.TestNS,
		ImagePath: o.Image, ImageURL: o.ImageURL, ImageDigest: o.ImageDigest, Arch: o.Arch,
		Bridge: o.Bridge, PVC: o.PVC, NoVM: o.NoVM, Keep: o.Keep, BootTimeout: o.BootTimeout, RunID: runID,
		ControlPlane: func(ctx context.Context) []selftest.Check {
			return doctorAsChecks(runDoctor(ctx, kc, &doctorOpts{Namespace: o.InstallNS, ReleaseName: "kairon"}))
		},
	}
	if uiBaseURL() != "" {
		env.SerialLog = uiSerialLog
		env.Metrics = uiMetricSamples
	}

	var reporters multiReporter
	var term *termReporter
	if o.Output == "" {
		term = newTermReporter(out)
		defer term.Close()
		reporters = append(reporters, term)
		term.Banner(profile, "run "+runID, "namespace "+o.TestNS, "kaironctl "+version, time.Now().Format("2006-01-02 15:04:05"))
	}
	var logFile *os.File
	if o.LogFile != "" {
		logFile, err = os.Create(o.LogFile)
		if err != nil {
			return fmt.Errorf("--log-file: %w", err)
		}
		defer func() { _ = logFile.Close() }()
		fr := &termReporter{w: logFile, start: time.Now(), now: time.Now}
		reporters = append(reporters, fr)
		fr.Banner(profile, "run "+runID, "namespace "+o.TestNS)
	}
	if len(reporters) == 0 {
		reporters = append(reporters, quietReporter{})
	}

	results := selftest.Run(ctx, env, reporters, selftest.Options{Profile: profile, Only: o.Only, Skip: o.Skip})

	if o.Output == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"runId": runID, "profile": profile, "summary": selftest.Summarize(results), "results": results,
		})
	} else {
		term.Summary(results, o.Strict)
	}
	if logFile != nil {
		(&termReporter{w: logFile, start: time.Now(), now: time.Now}).Summary(results, o.Strict)
	}
	if selftest.Summarize(results).Fail > 0 && !o.NoEvidence && o.Output == "" {
		printEvidence(ctx, kc, out, o, runID)
	}
	if selftest.ExitCode(results, o.Strict) != 0 {
		return fmt.Errorf("self-test failed")
	}
	return nil
}

func listSelftest(w io.Writer) {
	_, _ = fmt.Fprintf(w, "%-16s %-9s %s\n", "ID", "PROFILE", "CHECKS")
	for _, sc := range selftest.List() {
		_, _ = fmt.Fprintf(w, "%-16s %-9s %s\n", sc.ID, sc.Profile, sc.Title)
	}
}

func newRunID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func doctorAsChecks(rep doctorReport) []selftest.Check {
	out := make([]selftest.Check, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		sc := selftest.Check{Name: c.Name, Detail: c.Detail, Hint: c.Hint, Status: selftest.Pass}
		switch c.Status {
		case checkFail:
			sc.Status = selftest.Fail
			// A 403 is the limit of the identity in use (for example the
			// kairon-node service account on a systemd host), not evidence
			// that Kairon is broken; report it as not checked.
			if isForbidden(c.Detail) {
				sc.Status = selftest.Skip
				sc.Detail = "not checked: the identity in use may not read this (HTTP 403); use an admin kubeconfig for the full check"
				sc.Hint = ""
			}
		case checkWarn:
			sc.Warn = true
		case checkSkip:
			sc.Status = selftest.Skip
		}
		out = append(out, sc)
	}
	return out
}

func isForbidden(detail string) bool {
	return strings.Contains(detail, "HTTP 403") || strings.Contains(strings.ToLower(detail), "is forbidden")
}

func cleanupSelftest(ctx context.Context, kc *kube.Client, w io.Writer) error {
	machines, err := kc.ListMachinesWithSelector(ctx, selftest.LabelKey)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		_, _ = fmt.Fprintln(w, "no self-test Machines found")
		return nil
	}
	var firstErr error
	for _, m := range machines {
		if err := kc.DeleteMachine(ctx, m.Namespace(), m.Metadata.Name); err != nil && !kube.IsNotFound(err) {
			if firstErr == nil {
				firstErr = err
			}
			_, _ = fmt.Fprintf(w, "could not delete %s/%s: %v\n", m.Namespace(), m.Metadata.Name, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "deleted %s/%s\n", m.Namespace(), m.Metadata.Name)
	}
	return firstErr
}

// printEvidence appends the controller and node log lines that mention this
// run's Machines, the part of a failure report people otherwise go hunting for.
func printEvidence(ctx context.Context, kc *kube.Client, w io.Writer, o *selftestOpts, runID string) {
	t := newTermReporter(w)
	_, _ = fmt.Fprintln(w, t.paint("\033[1;38;5;221m", " Evidence")+t.paint("\033[38;5;245m", fmt.Sprintf("  (component log lines mentioning kairon-selftest-%s)", runID)))
	needle := "kairon-selftest-" + runID
	found := 0
	for _, comp := range []string{"kairon-controller", "kairon-node"} {
		pods, err := listPods(ctx, kc, o.InstallNS, "app.kubernetes.io/name="+comp)
		if err != nil || len(pods) == 0 {
			continue
		}
		for _, p := range pods {
			rc, err := podLogs(ctx, kc, o.InstallNS, p.Name, logOptions{Tail: o.EvidenceLines})
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
			for sc.Scan() {
				if line := sc.Text(); strings.Contains(line, needle) {
					found++
					_, _ = fmt.Fprintf(w, "  %s %s\n", t.paint("\033[38;5;240m", comp+"│"), line)
				}
			}
			_ = rc.Close()
		}
	}
	if found == 0 {
		_, _ = fmt.Fprintln(w, t.paint("\033[38;5;245m", "  no matching lines. Components outside Kubernetes (systemd) log to the journal:"))
		_, _ = fmt.Fprintf(w, "  %s\n", "journalctl -u kairon-node -u kairon-controller | grep "+needle)
	}
	_, _ = fmt.Fprintf(w, "\n  %s %s\n\n", t.paint("\033[38;5;245m", "full bundle:"), "kaironctl sysdump -o selftest-"+runID+".tar.gz")
}

// --- dashboard API helpers (serial console, usage samples) -------------

func uiGet(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uiBaseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	if t := uiToken(true); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := uiDo(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func uiSerialLog(ctx context.Context, ns, name string) (string, error) {
	p := fmt.Sprintf("/api/v1/machines/%s/%s/logs?lines=400&follow=false", url.PathEscape(ns), url.PathEscape(name))
	b, err := uiGet(ctx, p)
	return string(b), err
}

func uiMetricSamples(ctx context.Context, ns, name string) (int, error) {
	p := fmt.Sprintf("/api/v1/machines/%s/%s/metrics?range=15m", url.PathEscape(ns), url.PathEscape(name))
	b, err := uiGet(ctx, p)
	if err != nil {
		return 0, err
	}
	var r struct {
		Samples []json.RawMessage `json:"samples"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	return len(r.Samples), nil
}

// --- small reporters ---------------------------------------------------

type multiReporter []selftest.Reporter

func (m multiReporter) Log(l selftest.Level, msg string) {
	for _, r := range m {
		r.Log(l, msg)
	}
}
func (m multiReporter) StepStart(id, title string) {
	for _, r := range m {
		r.StepStart(id, title)
	}
}
func (m multiReporter) StepDone(res selftest.Result) {
	for _, r := range m {
		r.StepDone(res)
	}
}

type quietReporter struct{}

func (quietReporter) Log(selftest.Level, string) {}
func (quietReporter) StepStart(string, string)   {}
func (quietReporter) StepDone(selftest.Result)   {}
