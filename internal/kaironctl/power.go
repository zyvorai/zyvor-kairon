// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdPower(ctx context.Context, kc *kube.Client, args []string, state string) {
	ns, args := nsFlag(args)
	if len(args) != 1 {
		fatal(fmt.Errorf("command requires NAME"))
	}
	if err := setPowerState(ctx, kc, ns, args[0], state); err != nil {
		fatal(err)
	}
	okf("machine/%s -> %s", args[0], state)
}

func setPowerState(ctx context.Context, kc *kube.Client, namespace, name, state string) error {
	return kc.PatchMachine(ctx, namespace, name, map[string]any{"spec": map[string]any{"powerState": state}})
}

// stopPatch sets powerState and, with --force or --grace-period, the
// Machine's stop grace period annotation (removed by --force).
func stopPatch(state string, force bool, grace int) (map[string]any, error) {
	if force && grace > 0 {
		return nil, fmt.Errorf("--force and --grace-period are mutually exclusive")
	}
	if grace < 0 {
		return nil, fmt.Errorf("--grace-period must be positive")
	}
	patch := map[string]any{"spec": map[string]any{"powerState": state}}
	switch {
	case force:
		patch["metadata"] = map[string]any{"annotations": map[string]any{model.AnnotationStopGracePeriod: nil}}
	case grace > 0:
		patch["metadata"] = map[string]any{"annotations": map[string]any{model.AnnotationStopGracePeriod: strconv.Itoa(grace)}}
	}
	return patch, nil
}

func newStopCmd(opts *Options, use, state string) *cobra.Command {
	var force bool
	var grace int
	cmd := &cobra.Command{
		Use:   use + " MACHINE",
		Short: "Set Machine powerState to " + state,
		Long: `Set the Machine's powerState to ` + state + `.

--grace-period N first asks the guest agent to power the guest off and stops
the runtime once it has, or after N seconds (at most 600). The period is kept
on the Machine (kairon.zyvor.dev/stop-grace-period-seconds) for later stops.
It needs spec.guestAgent.enabled; without a guest agent the stop is
immediate. --force stops immediately and removes a stored grace period.`,
		Example: `  $ kaironctl ` + use + ` web-1
  $ kaironctl ` + use + ` web-1 --grace-period 60
  $ kaironctl ` + use + ` web-1 --force`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch, err := stopPatch(state, force, grace)
			if err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], patch); err != nil {
				return err
			}
			msg := "machine/%s -> " + state
			if grace > 0 {
				msg += fmt.Sprintf(" (guest gets up to %ds to power off)", grace)
			}
			okf(msg, args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop immediately and clear a stored grace period")
	cmd.Flags().IntVar(&grace, "grace-period", 0, "seconds the guest gets to power itself off first")
	return cmd
}

func restartPatch(now time.Time) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		model.AnnotationRestartRequested: now.UTC().Format(time.RFC3339Nano),
	}}}
}

func newRestartCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "restart MACHINE",
		Aliases: []string{"reset"},
		Short:   "Power-cycle a Running Machine",
		Long: `Power-cycle a Running Machine: kairon-node stops its runtime and starts it again
from its disk, on the same node, keeping its identity and addresses. Like
virtctl restart; "reset" is the same command, because FluxVM's stop is already
an immediate power-off. Use soft-reboot to have the guest OS reboot itself.

Each call is a new request (kairon.zyvor.dev/restart-requested); kairon-node
records the last one it acted on in status.lastRestartRequest. A restart waits
while a migration of the Machine is in flight.

Unlike virtctl restart, the VM is not rebuilt from the current spec: settings
fixed when the runtime is created (such as spec.network.forwards) apply only
after stop and start, which also resets a Machine without spec.volumes to its
image.`,
		Example: `  $ kaironctl restart web-1
  $ kaironctl reset web-1`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			if s := m.DesiredPowerState(); s != "Running" {
				return fmt.Errorf("machine/%s has powerState %s; restart needs Running (use kaironctl start)", args[0], s)
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], restartPatch(time.Now())); err != nil {
				return err
			}
			okf("machine/%s restart requested", args[0])
			return nil
		},
	}
}

func newSoftRebootCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "soft-reboot MACHINE",
		Short: "Ask the guest OS to reboot itself through the guest agent",
		Long: `Ask the guest to reboot (systemctl reboot, or Restart-Computer on Windows) through
the guest agent, like virtctl soft-reboot. The VM keeps running; only the guest
OS restarts. Needs spec.guestAgent.enabled, KAIRON_UI_URL and an administrator
KAIRON_UI_TOKEN.`,
		Example:           `  $ kaironctl soft-reboot web-1`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := uiRaw(cmd.Context(), http.MethodPost, machinePath(opts.Namespace, args[0], "soft-reboot"), map[string]any{}); err != nil {
				return err
			}
			okf("machine/%s guest is rebooting", args[0])
			return nil
		},
	}
}
