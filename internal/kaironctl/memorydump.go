// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kaironctl/style"
)

func diagnosticsPath(ns, name, token string) string {
	p := fmt.Sprintf("/api/v1/machines/%s/%s/diagnostics", url.PathEscape(ns), url.PathEscape(name))
	if token != "" {
		p += "/" + url.PathEscape(token)
	}
	return p
}

// startMemoryDump starts a redacted FluxVM bundle with a memory dump and
// waits for it to finish.
func startMemoryDump(ctx context.Context, ns, name string) (*fluxvm.DiagnosticsBundle, error) {
	body, err := json.Marshal(fluxvm.DiagnosticsRequest{MemoryDump: true, Redact: true})
	if err != nil {
		return nil, err
	}
	var job fluxvm.DiagnosticsBundle
	if err := uiCall(ctx, http.MethodPost, diagnosticsPath(ns, name, ""), bytes.NewReader(body), &job); err != nil {
		return nil, fmt.Errorf("start memory dump: %w", err)
	}
	for job.State == "running" {
		select {
		case <-ctx.Done():
			return &job, fmt.Errorf("memory dump %s still running: %w", job.Token, ctx.Err())
		case <-time.After(incidentPoll):
		}
		if err := uiCall(ctx, http.MethodGet, diagnosticsPath(ns, name, job.Token), nil, &job); err != nil {
			return &job, fmt.Errorf("poll: %w", err)
		}
	}
	if job.State != "done" {
		return &job, fmt.Errorf("memory dump %s %s: %s", job.Token, job.State, job.Error)
	}
	if job.MemoryDump == nil {
		return &job, fmt.Errorf("bundle %s finished without a memory dump", job.Token)
	}
	return &job, nil
}

func newMemoryDumpCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory-dump",
		Short: "Dump a Machine's guest memory on its node, download it, or remove it",
		Long: `Like virtctl memory-dump. FluxVM has QEMU write the guest's memory as an ELF core
on the Machine's node (the guest pauses while it is written); download it with
"memory-dump download" and open it with crash or gdb. The node keeps the last
5 bundles per VM; "memory-dump remove" frees the space sooner.

Needs KAIRON_UI_URL and an administrator KAIRON_UI_TOKEN. QEMU runtimes only.`,
	}
	var timeout time.Duration
	var output string
	get := &cobra.Command{
		Use:   "get MACHINE",
		Short: "Dump guest memory on the node and print the token to download it",
		Example: `  $ kaironctl memory-dump get db
  $ kaironctl memory-dump get db -o db.elf   # dump and download in one go`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if uiBaseURL() == "" {
				return fmt.Errorf("needs KAIRON_UI_URL")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			style.Log(style.EmojiWait, "dumping %s/%s guest memory (the guest pauses while it is written)", opts.Namespace, args[0])
			job, err := startMemoryDump(ctx, opts.Namespace, args[0])
			if err != nil {
				return err
			}
			d := job.MemoryDump
			style.Log(style.EmojiOK, "memory dump %s: %s on the node (%d MiB, guest paused %dms, sha256 %s)", job.Token, d.Path, d.SizeBytes>>20, d.PausedMs, d.SHA256)
			if output == "" {
				fmt.Printf("download: kaironctl memory-dump download %s %s -n %s -o %s.elf\n", args[0], job.Token, opts.Namespace, args[0])
				return nil
			}
			n, sum, err := downloadMemoryDump(cmd.Context(), opts.Namespace, args[0], job.Token, output)
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "wrote %d bytes to %s (sha256 %s)", n, output, sum)
			return nil
		},
	}
	get.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "give up after this long")
	get.Flags().StringVarP(&output, "output", "o", "", "also download the dump to FILE")

	var dlOutput string
	download := &cobra.Command{
		Use:               "download MACHINE TOKEN",
		Short:             "Download a memory dump and check its sha256",
		Example:           `  $ kaironctl memory-dump download db 3f9c0a1b2c3d4e5f -o db.elf`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if uiBaseURL() == "" {
				return fmt.Errorf("needs KAIRON_UI_URL")
			}
			if dlOutput == "" {
				dlOutput = fmt.Sprintf("%s-%s.elf", args[0], args[1])
			}
			n, sum, err := downloadMemoryDump(cmd.Context(), opts.Namespace, args[0], args[1], dlOutput)
			if err != nil {
				return err
			}
			style.Log(style.EmojiOK, "wrote %d bytes to %s (sha256 %s)", n, dlOutput, sum)
			return nil
		},
	}
	download.Flags().StringVarP(&dlOutput, "output", "o", "", "dump file (default MACHINE-TOKEN.elf)")

	remove := &cobra.Command{
		Use:               "remove MACHINE TOKEN",
		Aliases:           []string{"rm", "delete"},
		Short:             "Remove a memory dump (and its bundle) from the node",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMachineNames(&opts.Namespace),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := uiRaw(cmd.Context(), http.MethodDelete, diagnosticsPath(opts.Namespace, args[0], args[1]), nil); err != nil {
				return err
			}
			okf("memory dump %s removed from %s/%s's node", args[1], opts.Namespace, args[0])
			return nil
		},
	}
	cmd.AddCommand(get, download, remove)
	return cmd
}
