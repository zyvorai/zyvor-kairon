// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const defaultUnfreezeTimeout = 5 * time.Minute

func freezePatch(now time.Time, timeout time.Duration) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		model.AnnotationFsFreezeUntil: now.Add(timeout).UTC().Format(time.RFC3339),
	}}}
}

func unfreezePatch() map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		model.AnnotationFsFreezeUntil: nil,
	}}}
}

// waitFrozen polls until kairon-node confirms (want true) or clears (want
// false) kairon.zyvor.dev/fs-frozen.
func waitFrozen(ctx context.Context, kc *kube.Client, ns, name string, want bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		m, err := kc.GetMachine(ctx, ns, name)
		if err != nil {
			return err
		}
		if (m.Metadata.Annotations[model.AnnotationFsFrozen] != "") == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kairon-node has not confirmed within %s; check `kaironctl events -n %s`", timeout, ns)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func newFreezeCmd(opts *Options) *cobra.Command {
	var timeout time.Duration
	var noWait bool
	cmd := &cobra.Command{
		Use:   "freeze MACHINE",
		Short: "Freeze the guest's filesystems through the guest agent",
		Long: `Freeze every mounted, writable filesystem in the guest (qemu-guest-agent
guest-fsfreeze-freeze), like virtctl freeze -- for a consistent backup taken
outside Kairon. Needs spec.guestAgent.enabled.

kairon-node holds the freeze until --unfreeze-timeout (default 5m) and then
thaws on its own, so a forgotten freeze cannot wedge the guest. Run
kaironctl unfreeze to thaw earlier. A snapshot quiesce takes precedence while
it runs; the freeze is restored afterwards.`,
		Example: `  $ kaironctl freeze db-1
  $ kaironctl freeze db-1 --unfreeze-timeout 15m`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--unfreeze-timeout must be positive")
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			m, err := kc.GetMachine(cmd.Context(), opts.Namespace, args[0])
			if err != nil {
				return err
			}
			if !m.Spec.GuestAgent.Enabled {
				return fmt.Errorf("machine/%s has no guest agent (spec.guestAgent.enabled)", args[0])
			}
			if m.Status.Phase != "Running" {
				return fmt.Errorf("machine/%s is %s; freeze needs Running", args[0], m.Status.Phase)
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], freezePatch(time.Now(), timeout)); err != nil {
				return err
			}
			if !noWait {
				if err := waitFrozen(cmd.Context(), kc, opts.Namespace, args[0], true, time.Minute); err != nil {
					return err
				}
			}
			okf("machine/%s filesystems frozen (auto-thaw in %s)", args[0], timeout)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "unfreeze-timeout", defaultUnfreezeTimeout, "thaw automatically after this long")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the request is recorded, without waiting for kairon-node")
	return cmd
}

func newUnfreezeCmd(opts *Options) *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:               "unfreeze MACHINE",
		Short:             "Thaw the guest's filesystems frozen by kaironctl freeze",
		Example:           `  $ kaironctl unfreeze db-1`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if err := kc.PatchMachine(cmd.Context(), opts.Namespace, args[0], unfreezePatch()); err != nil {
				return err
			}
			if !noWait {
				if err := waitFrozen(cmd.Context(), kc, opts.Namespace, args[0], false, time.Minute); err != nil {
					return err
				}
			}
			okf("machine/%s filesystems thawed", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return once the request is recorded, without waiting for kairon-node")
	return cmd
}
