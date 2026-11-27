// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"

	"github.com/spf13/cobra"
)

func newMachineSetCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machineset",
		Short: "MachineSet operations",
	}
	cmd.AddCommand(newMachineSetHistoryCmd(opts))
	cmd.AddCommand(newMachineSetRollbackCmd(opts))
	return cmd
}

func newMachineSetHistoryCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "history NAME",
		Short:   "Show the previous template hash recorded on a MachineSet",
		Example: machinesetHistoryExamples,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			cmdMachineSetHistory(context.Background(), kc, args)
			return nil
		},
	}
}

func newMachineSetRollbackCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:     "rollback NAME",
		Short:   "Show the previous template hash recorded for rollback",
		Example: machinesetRollbackExamples,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			cmdMachineSetRollback(context.Background(), kc, args)
			return nil
		},
	}
}
