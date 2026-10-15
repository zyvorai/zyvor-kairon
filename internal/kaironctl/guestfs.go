// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newGuestfsCmd(opts *Options) *cobra.Command {
	var rw bool
	cmd := &cobra.Command{
		Use:   "guestfs MACHINE",
		Short: "Open a guestfish shell on a Machine's disk",
		Long: `Run libguestfs' guestfish on the node against the Machine's disk and attach
your terminal to it, like virtctl guestfs. The guest OS is inspected and
mounted (-i). Read-only by default, which is safe while the Machine runs;
--rw needs the Machine Halted (kaironctl halt) so nothing else has the disk
open. Admin-only; needs libguestfs-tools on the node. Ctrl+] detaches.`,
		Example: `  $ kaironctl guestfs web-1
  ><fs> cat /etc/os-release
  $ kaironctl halt web-1 && kaironctl guestfs web-1 --rw`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, ns := args[0], opts.Namespace
			if kc, err := newKubeClient(); err == nil {
				if m, err := kc.GetMachine(cmd.Context(), ns, name); err == nil {
					switch {
					case m.Status.RuntimeID == "":
						return fmt.Errorf("machine/%s has no runtime (phase %s); a Stopped Machine keeps no disk -- halt it instead", name, orDash(m.Status.Phase))
					case rw && m.Status.Phase != "Halted":
						return fmt.Errorf("machine/%s is %s; --rw needs it Halted (kaironctl halt %s)", name, m.Status.Phase, name)
					}
				}
			}
			kind := "guestfs"
			if rw {
				kind = "guestfs-rw"
			}
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
			defer cancel()
			stdinFd := int(os.Stdin.Fd())
			cols, rows := 0, 0
			if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
				cols, rows = w, h
			}
			conn, err := dialConsole(ctx, ns, name, kind, cols, rows)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			restore := func() {}
			if term.IsTerminal(stdinFd) {
				state, err := term.MakeRaw(stdinFd)
				if err != nil {
					return fmt.Errorf("raw terminal: %w", err)
				}
				restore = func() { _ = term.Restore(stdinFd, state) }
				defer restore()
			}
			start := time.Now()
			why := runConsoleSession(ctx, conn, os.Stdin, os.Stdout, consoleEscape)
			restore()
			_, _ = fmt.Fprint(os.Stdout, consoleGoodbye(os.Stdout, why, ns, name, time.Since(start)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&rw, "rw", false, "open the disk read-write (Machine must be Halted)")
	return cmd
}
