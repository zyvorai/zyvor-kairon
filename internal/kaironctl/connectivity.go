// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

type connectivityOpts struct {
	Namespace string
	Timeout   time.Duration
	Output    string

	// Machine lifecycle test (opt-in; creates and deletes a real Machine).
	Machine        bool
	Image          string
	TestNamespace  string
	CPU            string
	Memory         string
	Backend        string
	Network        string
	MachineTimeout time.Duration
	Keep           bool
}

func newConnectivityCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "connectivity",
		Short: "Verify that Kairon's components are reachable",
	}
	o := &connectivityOpts{
		Namespace: "kairon-system", Timeout: 30 * time.Second,
		TestNamespace: "default", CPU: "1", Memory: "512Mi", Backend: "qemu", Network: "user", MachineTimeout: 3 * time.Minute,
	}
	test := &cobra.Command{
		Use:   "test",
		Short: "Probe every Kairon pod's /readyz and /healthz through the API server",
		Long: `For each controller, node agent and dashboard pod (and the dashboard Service) request
/healthz and /readyz through the Kubernetes API server's proxy, so the check needs no
port-forward and no cluster-internal address. It is read-only and non-disruptive.

It answers "can the API server reach each component and is it ready", which is the
usual first question.

With --machine (and --image) it additionally runs an end-to-end lifecycle check: it
creates a small Machine labelled kairon.zyvor.dev/connectivity-test=true, waits for it to
reach Running, then deletes it and waits for the deletion. This schedules a real VM on a
real node and counts against quotas; it is never run unless you ask for it. The Machine
is labelled so leftovers are easy to find (kaironctl get machines --selector
kairon.zyvor.dev/connectivity-test=true) and carries spec.ttlSeconds, which FluxVM
enforces on the node as a best-effort safety net; it is not a guarantee that the Machine
object disappears if kaironctl is killed.`,
		Example: `  $ kaironctl connectivity test
  $ kaironctl connectivity test -o json
  $ kaironctl connectivity test --machine --image /var/lib/fluxvm/images/ubuntu.qcow2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if o.Machine && o.Image == "" {
				return fmt.Errorf("--machine needs --image PATH (a host-local image the nodes can read)")
			}
			probeCtx, cancel := context.WithTimeout(ctx, o.Timeout)
			defer cancel()
			rep := runConnectivity(probeCtx, kc, o.Namespace)
			if o.Machine {
				runMachineLifecycle(ctx, kc, o, &rep)
				rep.Healthy = rep.Fail == 0
			}
			out := cmd.OutOrStdout()
			if o.Output != "" {
				if err := writeStructured(out, rep, o.Output); err != nil {
					return err
				}
			} else {
				writeDoctor(out, rep)
			}
			if rep.Fail > 0 {
				return fmt.Errorf("connectivity test: %d probe(s) failed", rep.Fail)
			}
			return nil
		},
	}
	test.Flags().StringVarP(&o.Namespace, "namespace", "n", o.Namespace, "namespace where Kairon is installed")
	test.Flags().DurationVar(&o.Timeout, "timeout", o.Timeout, "overall time limit")
	test.Flags().StringVarP(&o.Output, "output", "o", "", "output format: json|yaml (default: table)")
	test.Flags().BoolVar(&o.Machine, "machine", false, "also create, run and delete a real Machine (disruptive; needs --image)")
	test.Flags().StringVar(&o.Image, "image", "", "host-local image path for --machine")
	test.Flags().StringVar(&o.TestNamespace, "test-namespace", o.TestNamespace, "namespace for the --machine test Machine")
	test.Flags().StringVar(&o.CPU, "cpu", o.CPU, "vCPU quantity for the --machine test Machine")
	test.Flags().StringVar(&o.Memory, "memory", o.Memory, "memory quantity for the --machine test Machine")
	test.Flags().StringVar(&o.Backend, "backend", o.Backend, "runtime backend for the --machine test Machine")
	test.Flags().StringVar(&o.Network, "network", o.Network, "network mode for the --machine test Machine (user|tap|macvtap)")
	test.Flags().DurationVar(&o.MachineTimeout, "machine-timeout", o.MachineTimeout, "how long to wait for the test Machine to reach Running")
	test.Flags().BoolVar(&o.Keep, "keep", false, "do not delete the test Machine (for debugging)")
	root.AddCommand(test)
	return root
}

func runConnectivity(ctx context.Context, kc *kube.Client, ns string) doctorReport {
	rep := doctorReport{}
	probed := 0
	for _, c := range []struct{ label, component string }{
		{"controller", "kairon-controller"},
		{"node agent", "kairon-node"},
		{"dashboard", "kairon-ui"},
	} {
		pods, err := listPods(ctx, kc, ns, "app.kubernetes.io/name="+c.component)
		if err != nil {
			rep.add(doctorCheck{Name: c.label, Status: checkFail, Detail: err.Error(), Hint: "check -n/--namespace and RBAC"})
			continue
		}
		if len(pods) == 0 {
			rep.add(doctorCheck{Name: c.label, Status: checkSkip, Detail: "no pods in namespace " + ns + " (components running outside Kubernetes, e.g. systemd units from scripts/deploy-remote.sh, are not probed; use --machine for an end-to-end check)"})
			continue
		}
		for _, p := range pods {
			name := fmt.Sprintf("%s %s", c.label, p.Name)
			if p.HealthPort == 0 {
				rep.add(doctorCheck{Name: name, Status: checkSkip, Detail: "pod exposes no health port"})
				continue
			}
			target := fmt.Sprintf("%s:%d", p.Name, p.HealthPort)
			failed := false
			for _, probe := range []string{"/healthz", "/readyz"} {
				probed++
				if _, err := proxyGet(ctx, kc, "pods", ns, target, probe); err != nil {
					rep.add(doctorCheck{Name: name, Status: checkFail, Detail: probe + ": " + err.Error(),
						Hint: "kaironctl logs " + c.component[len("kairon-"):] + "; kaironctl events --warnings"})
					failed = true
					break
				}
			}
			if !failed {
				rep.add(doctorCheck{Name: name, Status: checkOK, Detail: fmt.Sprintf("/healthz and /readyz on :%d", p.HealthPort)})
			}
		}
	}
	// The dashboard Service port, as clients reach it.
	// The dashboard serves HTTPS by default; an install with ui.insecureHttp
	// serves plain HTTP, which is reported as a warning rather than hidden.
	if _, err := proxyGet(ctx, kc, "services", ns, "https:kairon-ui:18082", "/readyz"); err == nil {
		rep.add(doctorCheck{Name: "dashboard service", Status: checkOK, Detail: "https://kairon-ui:18082 /readyz"})
	} else if _, httpErr := proxyGet(ctx, kc, "services", ns, "http:kairon-ui:18082", "/readyz"); httpErr == nil {
		rep.add(doctorCheck{Name: "dashboard service", Status: checkWarn, Detail: "kairon-ui:18082 answers on plain HTTP only",
			Hint: "logins and tokens are clear text; remove ui.insecureHttp (the dashboard serves HTTPS by default)"})
	} else if !kube.IsNotFound(err) {
		rep.add(doctorCheck{Name: "dashboard service", Status: checkWarn, Detail: err.Error(),
			Hint: "the Service may use a different port (ui.service.port) or the dashboard is disabled"})
	}
	rep.Healthy = rep.Fail == 0 && probed > 0
	return rep
}

const connectivityLabel = "kairon.zyvor.dev/connectivity-test"

// machinePollInterval is the Machine status poll period; tests shorten it.
var machinePollInterval = 2 * time.Second

// runMachineLifecycle creates a small Machine, waits for Running and deletes
// it again, recording each step as a check. The Machine is always deleted
// (unless --keep), including when ctx is cancelled mid-way.
func runMachineLifecycle(ctx context.Context, kc *kube.Client, o *connectivityOpts, rep *doctorReport) {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	name := "kairon-connectivity-" + hex.EncodeToString(suffix)
	ns := o.TestNamespace
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{connectivityLabel: "true"}},
		Spec: model.MachineSpec{
			Image:      model.ImageSpec{Path: o.Image},
			Resources:  model.ResourceSpec{CPU: o.CPU, Memory: o.Memory},
			Runtime:    model.RuntimeSpec{Backend: o.Backend},
			Network:    model.NetworkSpec{Mode: o.Network},
			PowerState: "Running",
			// Best-effort safety net: passed to FluxVM, which expires the VM on
			// the node. Kairon's controller does not delete the Machine object
			// for it, so the label above is the way to find leftovers.
			TTLSeconds: int64((o.MachineTimeout + 5*time.Minute).Seconds()),
		},
	}
	started := time.Now()
	if _, err := kc.CreateMachine(ctx, ns, m); err != nil {
		rep.add(doctorCheck{Name: "Machine create", Status: checkFail, Detail: err.Error(),
			Hint: "check quotas, admission webhook decisions (kaironctl events -n " + ns + " --warnings) and RBAC"})
		return
	}
	rep.add(doctorCheck{Name: "Machine create", Status: checkOK, Detail: fmt.Sprintf("%s/%s", ns, name)})

	if !o.Keep {
		defer func() {
			// A fresh context: the caller's may already be cancelled.
			cctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			rep.add(cleanupMachine(cctx, kc, ns, name))
		}()
	} else {
		rep.add(doctorCheck{Name: "Machine cleanup", Status: checkSkip, Detail: fmt.Sprintf("--keep: %s/%s left running (delete it with kaironctl delete machine %s -n %s)", ns, name, name, ns)})
	}

	deadline := time.Now().Add(o.MachineTimeout)
	last := ""
	for {
		cur, err := kc.GetMachine(ctx, ns, name)
		if err == nil {
			last = cur.Status.Phase
			switch last {
			case "Running":
				rep.add(doctorCheck{Name: "Machine running", Status: checkOK,
					Detail: fmt.Sprintf("Running after %s on node %s, guest IP %s", time.Since(started).Round(time.Second), dash(cur.Status.NodeName), dash(cur.Status.GuestIP))})
				return
			case "Failed", "Error":
				rep.add(doctorCheck{Name: "Machine running", Status: checkFail, Detail: "phase " + last,
					Hint: "kaironctl describe machine " + name + " -n " + ns + "; kaironctl events -n " + ns + " --warnings; kaironctl logs node"})
				return
			}
		}
		if time.Now().After(deadline) {
			rep.add(doctorCheck{Name: "Machine running", Status: checkFail, Detail: fmt.Sprintf("not Running after %s (phase %q)", o.MachineTimeout, dash(last)),
				Hint: "kaironctl get nodes; kaironctl events -n " + ns + " --warnings; check --image is readable on a node"})
			return
		}
		select {
		case <-ctx.Done():
			rep.add(doctorCheck{Name: "Machine running", Status: checkFail, Detail: "interrupted: " + ctx.Err().Error()})
			return
		case <-time.After(machinePollInterval):
		}
	}
}

func cleanupMachine(ctx context.Context, kc *kube.Client, ns, name string) doctorCheck {
	if err := kc.DeleteMachine(ctx, ns, name); err != nil && !kube.IsNotFound(err) {
		return doctorCheck{Name: "Machine cleanup", Status: checkWarn, Detail: "delete failed: " + err.Error(),
			Hint: fmt.Sprintf("kaironctl delete machine %s -n %s", name, ns)}
	}
	for {
		if _, err := kc.GetMachine(ctx, ns, name); err != nil {
			if kube.IsNotFound(err) {
				return doctorCheck{Name: "Machine cleanup", Status: checkOK, Detail: "deleted"}
			}
		}
		select {
		case <-ctx.Done():
			return doctorCheck{Name: "Machine cleanup", Status: checkWarn, Detail: "delete requested but the Machine still exists",
				Hint: fmt.Sprintf("kaironctl get machines -n %s --selector %s=true", ns, connectivityLabel)}
		case <-time.After(machinePollInterval):
		}
	}
}
