// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type checkStatus string

const (
	checkOK   checkStatus = "ok"
	checkWarn checkStatus = "warn"
	checkFail checkStatus = "fail"
	checkSkip checkStatus = "skip"
)

type doctorCheck struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
	Hint   string      `json:"hint,omitempty"`
}

type doctorReport struct {
	Checks  []doctorCheck `json:"checks"`
	OK      int           `json:"ok"`
	Warn    int           `json:"warn"`
	Fail    int           `json:"fail"`
	Skip    int           `json:"skip"`
	Healthy bool          `json:"healthy"`
}

func (r *doctorReport) add(c doctorCheck) {
	r.Checks = append(r.Checks, c)
	switch c.Status {
	case checkOK:
		r.OK++
	case checkWarn:
		r.Warn++
	case checkFail:
		r.Fail++
	default:
		r.Skip++
	}
}

type doctorOpts struct {
	Namespace   string
	ReleaseName string
	PreInstall  bool
	Strict      bool
	Output      string
}

// crdGroup is the API group of every core Kairon CRD.
const crdGroup = "kairon.zyvor.dev"

// coreCRDs are the kairon.zyvor.dev CRDs the chart installs (plural names).
var coreCRDs = []string{
	"machines", "machinemigrations", "machinesnapshots", "machinesnapshotrestores", "machinesnapshotschedules",
	"machinesets", "machinepools", "machineclaims", "machinequotas", "machinedisruptionbudgets",
	"machineimages", "machineinstancetypes", "machinenetworkpolicies", "networksecuritygroups",
	"migrationpolicies", "machinebackups", "machinebackuprestores", "machinebackuppolicies",
}

func newDoctorCmd(opts *Options) *cobra.Command {
	d := &doctorOpts{Namespace: "kairon-system", ReleaseName: "kairon"}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that Kairon is installed and healthy, with a hint for every problem",
		Long: `Run read-only health checks against the cluster and the installed release:
API server, CRDs (present, v1 storage), controller and node workloads, pod health
and readiness through the API server proxy, capable nodes, the admission webhook,
the Helm release and migrations parked in NeedsRecovery.

Exits non-zero when any check fails (--strict: also on warnings). --pre-install
only runs the checks that make sense before Kairon exists.`,
		Example: `  $ kaironctl doctor
  $ kaironctl doctor --pre-install
  $ kaironctl doctor -o json --strict`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if d.Output != "" && d.Output != "json" && d.Output != "yaml" {
				return fmt.Errorf("unknown output %q (want json or yaml)", d.Output)
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			rep := runDoctor(ctx, kc, d)
			out := cmd.OutOrStdout()
			if d.Output != "" {
				if err := writeStructured(out, rep, d.Output); err != nil {
					return err
				}
			} else {
				writeDoctor(out, rep)
			}
			if rep.Fail > 0 || (d.Strict && rep.Warn > 0) {
				return fmt.Errorf("doctor found %d failure(s) and %d warning(s)", rep.Fail, rep.Warn)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&d.Namespace, "namespace", "n", d.Namespace, "namespace where Kairon is installed")
	cmd.Flags().StringVar(&d.ReleaseName, "helm-release-name", d.ReleaseName, "Helm release name")
	cmd.Flags().BoolVar(&d.PreInstall, "pre-install", false, "only run checks that apply before Kairon is installed")
	cmd.Flags().BoolVar(&d.Strict, "strict", false, "exit non-zero on warnings too")
	cmd.Flags().StringVarP(&d.Output, "output", "o", "", "output format: json|yaml (default: table)")
	return cmd
}

func runDoctor(ctx context.Context, kc *kube.Client, d *doctorOpts) doctorReport {
	rep := doctorReport{}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 1. API server.
	var ver struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := kubeGetJSON(ctx, kc, "/version", &ver); err != nil {
		rep.add(doctorCheck{Name: "API server", Status: checkFail, Detail: err.Error(),
			Hint: "check --kubeconfig/--context, KUBECONFIG or KAIRON_KUBE_URL and your network access"})
		rep.Healthy = false
		return rep // nothing else can work
	}
	rep.add(doctorCheck{Name: "API server", Status: checkOK, Detail: ver.GitVersion})

	// 2. Capable nodes.
	nodes, err := capableNodes(ctx, kc)
	switch {
	case err != nil:
		rep.add(doctorCheck{Name: "Capable nodes", Status: checkWarn, Detail: err.Error(),
			Hint: "the current identity cannot list nodes; check RBAC"})
	case nodes == 0:
		rep.add(doctorCheck{Name: "Capable nodes", Status: checkFail, Detail: "no node carries " + model.CapableLabel + "=true",
			Hint: "kubectl label node <name> " + model.CapableLabel + "=true"})
	default:
		rep.add(doctorCheck{Name: "Capable nodes", Status: checkOK, Detail: fmt.Sprintf("%d node(s) labelled %s=true", nodes, model.CapableLabel)})
	}

	// 3. CRDs.
	checkCRDs(ctx, kc, &rep, d.PreInstall)
	if d.PreInstall {
		rep.Healthy = rep.Fail == 0
		return rep
	}

	// 4. Helm release.
	checkRelease(&rep, d)

	// 5. Workloads and pods.
	checkWorkload(ctx, kc, &rep, d.Namespace, "Controller", "kairon-controller", "deployments", true)
	checkWorkload(ctx, kc, &rep, d.Namespace, "Node agent", "kairon-node", "daemonsets", true)
	checkWorkload(ctx, kc, &rep, d.Namespace, "Dashboard (kairon-ui)", "kairon-ui", "deployments", false)

	// 6. Admission webhook.
	checkWebhook(ctx, kc, &rep)

	// 7. Stuck migrations.
	if migs, err := kc.ListMachineMigrations(ctx); err == nil {
		stuck := 0
		for _, m := range migs {
			if m.Status.Phase == "NeedsRecovery" {
				stuck++
			}
		}
		if stuck > 0 {
			rep.add(doctorCheck{Name: "Migrations", Status: checkWarn, Detail: fmt.Sprintf("%d migration(s) parked in NeedsRecovery", stuck),
				Hint: "kaironctl get migrations; resolve with kaironctl recover (see docs/runbook-migration-failures.md)"})
		} else {
			rep.add(doctorCheck{Name: "Migrations", Status: checkOK, Detail: fmt.Sprintf("%d migration(s), none parked", len(migs))})
		}
	}

	rep.Healthy = rep.Fail == 0
	return rep
}

func capableNodes(ctx context.Context, kc *kube.Client) (int, error) {
	var nl struct {
		Items []json.RawMessage `json:"items"`
	}
	path := "/api/v1/nodes?labelSelector=" + url.QueryEscape(model.CapableLabel+"=true")
	if err := kubeGetJSON(ctx, kc, path, &nl); err != nil {
		return 0, err
	}
	return len(nl.Items), nil
}

func checkCRDs(ctx context.Context, kc *kube.Client, rep *doctorReport, preInstall bool) {
	var missing, notV1 []string
	for _, plural := range coreCRDs {
		var crd struct {
			Spec struct {
				Versions []struct {
					Name    string `json:"name"`
					Served  bool   `json:"served"`
					Storage bool   `json:"storage"`
				} `json:"versions"`
			} `json:"spec"`
		}
		path := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + plural + "." + crdGroup
		if err := kubeGetJSON(ctx, kc, path, &crd); err != nil {
			if kube.IsNotFound(err) {
				missing = append(missing, plural)
				continue
			}
			rep.add(doctorCheck{Name: "CRDs", Status: checkWarn, Detail: err.Error(), Hint: "the current identity cannot read CustomResourceDefinitions; check RBAC"})
			return
		}
		storage := ""
		for _, v := range crd.Spec.Versions {
			if v.Storage {
				storage = v.Name
			}
		}
		if storage != "v1" {
			notV1 = append(notV1, plural+"("+storage+")")
		}
	}
	switch {
	case len(missing) == len(coreCRDs) && preInstall:
		rep.add(doctorCheck{Name: "CRDs", Status: checkOK, Detail: "not installed yet (expected before install)"})
	case len(missing) > 0:
		rep.add(doctorCheck{Name: "CRDs", Status: checkFail, Detail: "missing: " + strings.Join(missing, ", "),
			Hint: "kubectl apply --server-side -f deploy/crd.yaml (Helm only installs CRDs on first install)"})
	case len(notV1) > 0:
		rep.add(doctorCheck{Name: "CRDs", Status: checkWarn, Detail: "storage version is not v1: " + strings.Join(notV1, ", "),
			Hint: "apply deploy/crd.yaml from the release you are running before upgrading binaries"})
	default:
		rep.add(doctorCheck{Name: "CRDs", Status: checkOK, Detail: fmt.Sprintf("%d CRDs, storage version v1", len(coreCRDs))})
	}
}

func checkRelease(rep *doctorReport, d *doctorOpts) {
	rel, err := getReleaseFn(&releaseRef{Name: d.ReleaseName, Namespace: d.Namespace})
	if err != nil || rel == nil {
		rep.add(doctorCheck{Name: "Helm release", Status: checkSkip, Detail: fmt.Sprintf("release %q not readable (installed with raw manifests, or no access)", d.ReleaseName)})
		return
	}
	detail := describeRelease(&releaseRef{Name: d.ReleaseName, Namespace: d.Namespace})
	if rel.Info != nil && string(rel.Info.Status) != "deployed" {
		rep.add(doctorCheck{Name: "Helm release", Status: checkFail, Detail: detail,
			Hint: "kaironctl history; kaironctl rollback if the last upgrade failed"})
		return
	}
	rep.add(doctorCheck{Name: "Helm release", Status: checkOK, Detail: detail})
}

// checkWorkload verifies a Deployment/DaemonSet and probes /readyz on each of
// its pods through the API server proxy. optional workloads report skip when absent.
func checkWorkload(ctx context.Context, kc *kube.Client, rep *doctorReport, ns, label, name, kind string, required bool) {
	var ws workloadStatus
	if kind == "daemonsets" {
		ws = getDaemonSetStatus(ctx, kc, ns, name)
	} else {
		ws = getDeploymentStatus(ctx, kc, ns, name)
	}
	if ws.Err != "" {
		if strings.Contains(ws.Err, "404") && !required {
			rep.add(doctorCheck{Name: label, Status: checkSkip, Detail: "not deployed"})
			return
		}
		if strings.Contains(ws.Err, "404") {
			// Not a Kubernetes workload in this namespace. That is how a
			// binary/systemd install (scripts/deploy-remote.sh) looks, so it
			// cannot be told apart from "not installed" through the API:
			// warn instead of failing, and say how to check.
			rep.add(doctorCheck{Name: label, Status: checkWarn, Detail: fmt.Sprintf("no %s %s in namespace %s; cannot verify from the API", strings.ToLower(ws.Kind), name, ns),
				Hint: "installed as systemd services (scripts/deploy-remote.sh)? run: systemctl status " + name + "; otherwise kaironctl install, or check -n/--namespace"})
			return
		}
		rep.add(doctorCheck{Name: label, Status: checkFail, Detail: ws.Err,
			Hint: "kaironctl install, or check -n/--namespace"})
		return
	}
	if ws.Desired == 0 || ws.Ready < ws.Desired {
		rep.add(doctorCheck{Name: label, Status: checkFail, Detail: fmt.Sprintf("%s/%s ready %d/%d", ws.Kind, ws.Name, ws.Ready, ws.Desired),
			Hint: "kaironctl logs " + strings.TrimPrefix(name, "kairon-") + " --previous; kaironctl events"})
		return
	}
	pods, err := listPods(ctx, kc, ns, "app.kubernetes.io/name="+name)
	if err != nil {
		rep.add(doctorCheck{Name: label, Status: checkOK, Detail: fmt.Sprintf("%s/%s ready %d/%d (pods not listable: %v)", ws.Kind, ws.Name, ws.Ready, ws.Desired, err)})
		return
	}
	var problems []string
	status := checkOK
	for _, p := range pods {
		if p.Restarts >= 3 {
			problems = append(problems, fmt.Sprintf("%s restarted %d times", p.Name, p.Restarts))
			status = checkWarn
		}
		if p.HealthPort == 0 {
			continue
		}
		if _, err := proxyGet(ctx, kc, "pods", ns, fmt.Sprintf("%s:%d", p.Name, p.HealthPort), "/readyz"); err != nil {
			problems = append(problems, fmt.Sprintf("%s /readyz: %v", p.Name, err))
			status = checkFail
		}
	}
	detail := fmt.Sprintf("%s/%s ready %d/%d", ws.Kind, ws.Name, ws.Ready, ws.Desired)
	hint := ""
	if len(problems) > 0 {
		detail += "; " + strings.Join(problems, "; ")
		hint = "kaironctl logs " + strings.TrimPrefix(name, "kairon-") + " --previous; kaironctl events"
	}
	rep.add(doctorCheck{Name: label, Status: status, Detail: detail, Hint: hint})
}

func checkWebhook(ctx context.Context, kc *kube.Client, rep *doctorReport) {
	var vwc struct {
		Webhooks []struct {
			Name          string `json:"name"`
			FailurePolicy string `json:"failurePolicy"`
			ClientConfig  struct {
				CABundle string `json:"caBundle"`
			} `json:"clientConfig"`
		} `json:"webhooks"`
	}
	path := "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/kairon-controller-webhook"
	if err := kubeGetJSON(ctx, kc, path, &vwc); err != nil {
		if kube.IsNotFound(err) {
			rep.add(doctorCheck{Name: "Admission webhook", Status: checkSkip, Detail: "not enabled (webhook.enabled=false)"})
			return
		}
		rep.add(doctorCheck{Name: "Admission webhook", Status: checkWarn, Detail: err.Error()})
		return
	}
	var noCA, ignore []string
	for _, w := range vwc.Webhooks {
		if w.ClientConfig.CABundle == "" {
			noCA = append(noCA, w.Name)
		}
		if w.FailurePolicy == "Ignore" {
			ignore = append(ignore, w.Name)
		}
	}
	switch {
	case len(noCA) > 0:
		rep.add(doctorCheck{Name: "Admission webhook", Status: checkFail, Detail: "no caBundle on: " + strings.Join(noCA, ", "),
			Hint: "set webhook.caBundle (and webhook.tlsSecretName) in the chart values"})
	case len(ignore) > 0:
		rep.add(doctorCheck{Name: "Admission webhook", Status: checkWarn, Detail: "failurePolicy=Ignore: validation is skipped when the webhook is down",
			Hint: "use webhook.failurePolicy=Fail for production (required by fleet automation)"})
	default:
		rep.add(doctorCheck{Name: "Admission webhook", Status: checkOK, Detail: fmt.Sprintf("%d webhook(s), CA bundle set, failurePolicy=Fail", len(vwc.Webhooks))})
	}
}

func writeDoctor(w io.Writer, rep doctorReport) {
	for _, c := range rep.Checks {
		icon, color := "✓", style.Green
		switch c.Status {
		case checkWarn:
			icon, color = "!", style.Yellow
		case checkFail:
			icon, color = "✗", style.Red
		case checkSkip:
			icon, color = "-", style.Cyan
		}
		_, _ = fmt.Fprintf(w, "%s %-24s %s\n", style.Wrap(w, color, icon), c.Name, c.Detail)
		if c.Hint != "" {
			_, _ = fmt.Fprintf(w, "    hint: %s\n", c.Hint)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d ok, %d warning(s), %d failure(s), %d skipped\n", rep.OK, rep.Warn, rep.Fail, rep.Skip)
}
