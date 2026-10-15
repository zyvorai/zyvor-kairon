// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/zyvorai/kairon/internal/rehearsal"
)

func rehearsalExec(ctx context.Context, ns, name string, step rehearsal.Step) (rehearsal.ExecResult, error) {
	raw, err := uiRaw(ctx, http.MethodPost, machinePath(ns, name, "exec"), map[string]any{"path": step.Path, "args": step.Args, "timeoutSeconds": step.TimeoutSeconds})
	if err != nil {
		return rehearsal.ExecResult{}, err
	}
	// Do not accept an empty/malformed success response as exit code zero.
	var response struct {
		ExitCode *int64 `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return rehearsal.ExecResult{}, err
	}
	if response.ExitCode == nil {
		return rehearsal.ExecResult{}, fmt.Errorf("guest executor omitted exitCode")
	}
	return rehearsal.ExecResult{ExitCode: *response.ExitCode, Stdout: response.Stdout, Stderr: response.Stderr}, nil
}

func newRehearsalCmd() *cobra.Command {
	var planFile, reportFile string
	cmd := &cobra.Command{Use: "rehearsal", Short: "Rehearse application upgrades on isolated CSI-backed VM copies (experimental)"}
	run := &cobra.Command{Use: "run", Args: cobra.NoArgs, Short: "Restore copies, run baseline/change/verification, and clean up", RunE: func(cmd *cobra.Command, _ []string) error {
		f, err := os.Open(planFile)
		if err != nil {
			return err
		}
		p, err := rehearsal.Decode(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if uiBaseURL() == "" {
			return fmt.Errorf("KAIRON_UI_URL is required for guest execution")
		}
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		// Reserve the journal before any remote writes; never overwrite prior evidence.
		journal, err := os.OpenFile(reportFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if err := journal.Close(); err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		r := rehearsal.Runner{Kube: kc, Exec: rehearsalExec, Save: func(out rehearsal.Report) error { return rehearsal.WriteReport(reportFile, out) }}
		out, err := r.Run(ctx, p)
		_, printErr := fmt.Fprintf(cmd.OutOrStdout(), "rehearsal %s: %s; cleaned=%t; evidence=%s\n", out.RunID, out.Phase, out.Cleaned, reportFile)
		if err != nil {
			return err
		}
		return printErr
	}}
	run.Flags().StringVar(&planFile, "file", "", "JSON rehearsal plan")
	run.Flags().StringVar(&reportFile, "report", "", "new private JSON evidence/journal path")
	_ = run.MarkFlagRequired("file")
	_ = run.MarkFlagRequired("report")
	var cleanupFile string
	cleanup := &cobra.Command{Use: "cleanup", Args: cobra.NoArgs, Short: "Retry cleanup from a rehearsal journal after interruption", RunE: func(cmd *cobra.Command, _ []string) error {
		b, err := os.ReadFile(cleanupFile)
		if err != nil {
			return err
		}
		var out rehearsal.Report
		if err := json.Unmarshal(b, &out); err != nil {
			return err
		}
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		r := rehearsal.Runner{Kube: kc, Save: func(out rehearsal.Report) error { return rehearsal.WriteReport(cleanupFile, out) }}
		return r.Cleanup(cmd.Context(), &out)
	}}
	cleanup.Flags().StringVar(&cleanupFile, "report", "", "existing rehearsal evidence/journal path")
	_ = cleanup.MarkFlagRequired("report")
	cmd.AddCommand(run, cleanup)
	return cmd
}
