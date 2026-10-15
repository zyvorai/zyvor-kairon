// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// ExitCodeError makes kaironctl exit with Code and print nothing, so a
// command that wraps a program (container run --wait) can pass its status on.
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// containerFlags are the docker-style flags of `container run`.
type containerFlags struct {
	name, platform, workdir, user, entrypoint, restart, profile string
	cpu, memory, healthCmd                                      string
	env, publish, allowHosts, nodeSelector                      []string
	maxRestarts                                                 int32
	healthInterval                                              uint32
	offline, writableRoot, wait, rm, detach                     bool
	ttl                                                         int64
	timeout                                                     time.Duration
}

func newContainerCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "container",
		Aliases: []string{"ctr"},
		Short:   "Run OCI container images as FluxVM sandboxes (experimental)",
		Long: `Run a container image in its own lightweight VM on a FluxVM node, with a
docker-like CLI. No Docker daemon, Podman or Colima is involved: the image's
root filesystem is mounted by FluxVM's own init.

  kaironctl container run IMAGE [COMMAND...]    create (and with --wait, run to completion)
  kaironctl container ps [-a]                   list container Machines
  kaironctl container inspect NAME              spec and process state as JSON
  kaironctl container rm NAME...                delete container Machines

A container is an ordinary Machine with spec.sandbox.oci, so quotas,
placement, TTL, network policy and the dashboard all apply. FluxVM runs
the container track on macOS hosts; select such nodes with --node-selector.`,
	}
	cmd.AddCommand(newContainerRunCmd(opts), newContainerPsCmd(opts), newContainerInspectCmd(opts), newContainerRmCmd(opts))
	return cmd
}

func newContainerRunCmd(opts *Options) *cobra.Command {
	f := &containerFlags{}
	cmd := &cobra.Command{
		Use:   "run [flags] IMAGE [COMMAND [ARG...]]",
		Long:  "Flags come before IMAGE, as with docker run; everything after IMAGE is the container's command.",
		Short: "Create a container Machine from an OCI image",
		Example: `  $ kaironctl container run --wait --rm alpine:3.22 sh -c 'echo hi; exit 3'
  $ kaironctl container run -p 8080:80 --restart always --name web nginx:alpine
  $ kaironctl container run --platform linux/amd64 --wait alpine:3.22 uname -m`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := f.machine(args[0], args[1:], cmd.Flags().Changed("max-restarts"))
			if err != nil {
				return err
			}
			if err := model.ValidateSandboxOCI(m.Spec); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
			defer cancel()
			return runContainer(ctx, cmd.OutOrStdout(), kc, opts.Namespace, m, f)
		},
	}
	fl := cmd.Flags()
	fl.SetInterspersed(false) // everything after IMAGE is the container's command
	fl.StringVar(&f.name, "name", "", "Machine name (default: ctr-<random>)")
	fl.StringArrayVarP(&f.env, "env", "e", nil, "KEY=value (repeatable)")
	fl.StringArrayVarP(&f.publish, "publish", "p", nil, "publish a TCP port, HOST:CONTAINER (repeatable; host port 1024+)")
	fl.StringVarP(&f.workdir, "workdir", "w", "", "working directory inside the container")
	fl.StringVarP(&f.user, "user", "u", "", "uid[:gid] or name[:group]")
	fl.StringVar(&f.entrypoint, "entrypoint", "", "replace the image's entrypoint (space-separated argv)")
	fl.StringVar(&f.platform, "platform", "", "linux/arm64 (default) or linux/amd64 (Rosetta)")
	fl.StringVar(&f.restart, "restart", "", "no, on-failure or always")
	fl.Int32Var(&f.maxRestarts, "max-restarts", 0, "stop restarting after this many restarts")
	fl.StringVar(&f.healthCmd, "health-cmd", "", "health check command (space-separated argv)")
	fl.Uint32Var(&f.healthInterval, "health-interval", 0, "seconds between health checks")
	fl.BoolVar(&f.offline, "offline", false, "no network card")
	fl.StringArrayVar(&f.allowHosts, "allow-host", nil, "allow egress to HOST only (repeatable; implies offline)")
	fl.BoolVar(&f.writableRoot, "writable-root", false, "mount the image's root read-write")
	fl.StringVar(&f.profile, "profile", "", "FluxVM size class: tiny, small or standard (when --cpu/--memory are not set)")
	fl.StringVar(&f.cpu, "cpu", "", "vCPUs, e.g. 2")
	fl.StringVar(&f.memory, "memory", "", "memory, at least 512Mi, e.g. 1Gi")
	fl.StringArrayVar(&f.nodeSelector, "node-selector", nil, "KEY=value placement label (repeatable), e.g. to pick macOS nodes")
	fl.Int64Var(&f.ttl, "ttl-seconds", 0, "delete the Machine after this many seconds")
	fl.BoolVar(&f.wait, "wait", false, "wait for the process to exit and exit with its exit code")
	fl.BoolVar(&f.rm, "rm", false, "delete the Machine after it exits (implies --wait)")
	fl.BoolVarP(&f.detach, "detach", "d", false, "return as soon as the Machine is created")
	fl.DurationVar(&f.timeout, "timeout", 10*time.Minute, "give up waiting after this long")
	return cmd
}

// machine builds the container Machine from the flags and positional args.
func (f *containerFlags) machine(image string, command []string, maxRestartsSet bool) (model.Machine, error) {
	name := f.name
	if name == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return model.Machine{}, err
		}
		name = "ctr-" + hex.EncodeToString(b)
	}
	o := &model.OCISpec{
		Image: image, Platform: f.platform, Command: command, Env: f.env, WorkDir: f.workdir,
		User: f.user, WritableRoot: f.writableRoot, Ports: f.publish, Restart: f.restart,
		Offline: f.offline, AllowHosts: f.allowHosts, Profile: f.profile,
	}
	if f.entrypoint != "" {
		o.Entrypoint = strings.Fields(f.entrypoint)
	}
	if maxRestartsSet {
		v := f.maxRestarts
		o.MaxRestarts = &v
	}
	if f.healthCmd != "" {
		o.HealthCheck = &model.ContainerHealthCheck{Command: strings.Fields(f.healthCmd), IntervalSeconds: f.healthInterval}
	}
	if f.rm {
		// --rm waits for the process to exit and removes the Machine; the
		// VM has no reason to linger for exec afterwards.
		o.ExitPolicy = "poweroff"
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name},
		Spec: model.MachineSpec{
			PowerState: "Running",
			Resources:  model.ResourceSpec{CPU: f.cpu, Memory: f.memory},
			TTLSeconds: f.ttl,
			Sandbox:    &model.SandboxSpec{OCI: o},
		},
	}
	if len(f.nodeSelector) > 0 {
		m.Spec.Placement.NodeSelector = map[string]string{}
		for _, kv := range f.nodeSelector {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return model.Machine{}, fmt.Errorf("--node-selector %q must be KEY=value", kv)
			}
			m.Spec.Placement.NodeSelector[k] = v
		}
	}
	return m, nil
}

// containerAPI is the slice of the kube client run/ps/rm use, so the
// command logic is testable against a fake.
type containerAPI interface {
	CreateMachine(ctx context.Context, ns string, m model.Machine) (model.Machine, error)
	GetMachine(ctx context.Context, ns, name string) (model.Machine, error)
	DeleteMachine(ctx context.Context, ns, name string) error
	ListMachinesNamespace(ctx context.Context, ns string) ([]model.Machine, error)
}

var _ containerAPI = (*kube.Client)(nil)

// containerPollInterval is how often run polls Machine status.
var containerPollInterval = 2 * time.Second

func runContainer(ctx context.Context, out io.Writer, api containerAPI, ns string, m model.Machine, f *containerFlags) error {
	created, err := api.CreateMachine(ctx, ns, m)
	if err != nil {
		return fmt.Errorf("create container Machine: %w", err)
	}
	name := created.Metadata.Name
	if name == "" {
		name = m.Metadata.Name
	}
	_, _ = fmt.Fprintf(out, "container %s created in %s\n", name, ns)
	if f.detach || (!f.wait && !f.rm) {
		return nil
	}
	cur, err := waitContainerExit(ctx, api, ns, name)
	if f.rm {
		if derr := api.DeleteMachine(context.Background(), ns, name); derr != nil && err == nil {
			err = fmt.Errorf("remove container Machine: %w", derr)
		}
	}
	if err != nil {
		return err
	}
	code := 0
	if cur.Status.Container != nil && cur.Status.Container.ExitCode != nil {
		code = int(*cur.Status.Container.ExitCode)
	}
	_, _ = fmt.Fprintf(out, "container %s exited with code %d\n", name, code)
	if code != 0 {
		return &ExitCodeError{Code: code}
	}
	return nil
}

// waitContainerExit polls until the container process has an exit code, the
// Machine reports an init error or an Error phase, or ctx ends.
func waitContainerExit(ctx context.Context, api containerAPI, ns, name string) (model.Machine, error) {
	t := time.NewTicker(containerPollInterval)
	defer t.Stop()
	for {
		m, err := api.GetMachine(ctx, ns, name)
		if err != nil {
			return m, fmt.Errorf("read container Machine: %w", err)
		}
		if c := m.Status.Container; c != nil {
			if c.InitError != "" {
				return m, fmt.Errorf("container init failed: %s", c.InitError)
			}
			if c.ExitCode != nil {
				return m, nil
			}
		}
		if m.Status.Phase == "Error" {
			return m, fmt.Errorf("container Machine is in phase Error: %s", m.Status.Message)
		}
		select {
		case <-ctx.Done():
			return m, fmt.Errorf("timed out waiting for container %s to exit (phase %q)", name, m.Status.Phase)
		case <-t.C:
		}
	}
}

func containerMachines(ms []model.Machine) []model.Machine {
	var out []model.Machine
	for _, m := range ms {
		if m.Spec.Sandbox != nil && m.Spec.Sandbox.OCI != nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func containerState(m model.Machine) string {
	if c := m.Status.Container; c != nil && c.ExitCode != nil {
		return fmt.Sprintf("Exited (%d)", *c.ExitCode)
	}
	if m.Status.Phase == "" {
		return "Pending"
	}
	return m.Status.Phase
}

func newContainerPsCmd(opts *Options) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List container Machines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ms, err := kc.ListMachinesNamespace(ctx, opts.Namespace)
			if err != nil {
				return err
			}
			return writeContainerTable(cmd.OutOrStdout(), containerMachines(ms), all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include containers that have exited")
	return cmd
}

func writeContainerTable(out io.Writer, ms []model.Machine, all bool) error {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tIMAGE\tSTATE\tNODE\tPORTS")
	for _, m := range ms {
		if !all && m.Status.Container != nil && m.Status.Container.ExitCode != nil {
			continue
		}
		o := m.Spec.Sandbox.OCI
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Metadata.Name, o.Image, containerState(m), m.Status.NodeName, strings.Join(o.Ports, ","))
	}
	return tw.Flush()
}

func newContainerInspectCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect NAME",
		Short: "Show a container's spec and process state as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			m, err := kc.GetMachine(ctx, opts.Namespace, args[0])
			if err != nil {
				return err
			}
			if m.Spec.Sandbox == nil || m.Spec.Sandbox.OCI == nil {
				return fmt.Errorf("machine %s/%s is not a container (no spec.sandbox.oci)", opts.Namespace, args[0])
			}
			b, err := json.MarshalIndent(map[string]any{
				"name": m.Metadata.Name, "namespace": opts.Namespace, "phase": m.Status.Phase, "node": m.Status.NodeName,
				"message": m.Status.Message, "oci": m.Spec.Sandbox.OCI, "container": m.Status.Container,
			}, "", "  ")
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
}

func newContainerRmCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "rm NAME...",
		Aliases: []string{"delete"},
		Short:   "Delete container Machines",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return removeContainers(ctx, cmd.OutOrStdout(), kc, opts.Namespace, args)
		},
	}
}

// removeContainers deletes the named container Machines, refusing any that
// is not one so rm cannot be used to delete an arbitrary VM by mistake.
func removeContainers(ctx context.Context, out io.Writer, api containerAPI, ns string, names []string) error {
	var failed []string
	for _, n := range names {
		m, err := api.GetMachine(ctx, ns, n)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", n, err))
			continue
		}
		if m.Spec.Sandbox == nil || m.Spec.Sandbox.OCI == nil {
			failed = append(failed, fmt.Sprintf("%s: not a container Machine (use kaironctl delete)", n))
			continue
		}
		if err := api.DeleteMachine(ctx, ns, n); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", n, err))
			continue
		}
		_, _ = fmt.Fprintf(out, "container %s removed\n", n)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return nil
}
