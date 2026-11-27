// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

func newMachineSetCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machineset",
		Short: "MachineSet operations",
	}
	cmd.AddCommand(newMachineSetHistoryCmd(opts))
	cmd.AddCommand(newMachineSetStatusCmd(opts))
	cmd.AddCommand(newMachineSetRollbackCmd(opts))
	return cmd
}

// MachineSet subcommands take the namespace from the root -n/--namespace flag.
func newMachineSetHistoryCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "history NAME",
		Short:   "Show the template revisions recorded on a MachineSet",
		Example: machinesetHistoryExamples,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			return machineSetHistory(cmd.Context(), kc, opts.Namespace, args[0], cmd.OutOrStdout())
		},
	}
}

func newMachineSetRollbackCmd(opts *Options) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "rollback NAME",
		Short: "Restore a MachineSet's previous template",
		Long: `Restore the previous spec.template recorded by kairon-controller and swap the
current/previous revision annotations in one resourceVersion-guarded PATCH, so
running rollback again goes forward to the template you left. Replicas are then
replaced by the normal rollout (strategy, maxUnavailable, maxSurge).`,
		Example: machinesetRollbackExamples,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			from, to, err := rollbackMachineSet(cmd.Context(), kc, opts.Namespace, args[0], dryRun)
			if err != nil {
				return err
			}
			if dryRun {
				style.Log(style.EmojiOK, "machineset/%s would roll back template %s -> %s (dry-run, not changed)", args[0], from, to)
				return nil
			}
			style.Log(style.EmojiOK, "machineset/%s rolled back template %s -> %s", args[0], from, to)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be restored without changing anything")
	return cmd
}

func newMachineSetStatusCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "status NAME",
		Short:   "Show a MachineSet's replica counts and conditions",
		Example: machinesetStatusExamples,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			return machineSetStatus(cmd.Context(), kc, opts.Namespace, args[0], cmd.OutOrStdout(), time.Now())
		},
	}
}
