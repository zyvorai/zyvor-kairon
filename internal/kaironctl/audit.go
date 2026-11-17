// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// auditEntry is one MachineMigration reduced to what an operator reviewing
// recent moves wants to see.
type auditEntry struct {
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	Machine     string    `json:"machine"`
	Created     time.Time `json:"created"`
	Phase       string    `json:"phase"`
	Strategy    string    `json:"strategy"`
	Source      string    `json:"sourceNode,omitempty"`
	Target      string    `json:"targetNode,omitempty"`
	DowntimeMs  uint64    `json:"downtimeMs,omitempty"`
	TotalTimeMs uint64    `json:"totalTimeMs,omitempty"`
	Encrypted   bool      `json:"dataPlaneEncrypted,omitempty"`
	Message     string    `json:"message,omitempty"`
}

// auditReport is the -o json|yaml form: counts first, then the entries
// newest first.
type auditReport struct {
	Total   int            `json:"total"`
	ByPhase map[string]int `json:"byPhase"`
	Entries []auditEntry   `json:"migrations"`
}

// buildAudit summarises migrations created at or after since (a zero since
// keeps all), optionally limited to one Machine, newest first. limit <= 0
// keeps every entry; the counts always cover all matches.
func buildAudit(migs []model.MachineMigration, since time.Time, machine string, limit int) auditReport {
	rep := auditReport{ByPhase: map[string]int{}, Entries: []auditEntry{}}
	for _, m := range migs {
		if !since.IsZero() && m.Metadata.CreationTimestamp.Before(since) {
			continue
		}
		if machine != "" && m.Spec.MachineName != machine {
			continue
		}
		phase := m.Status.Phase
		if phase == "" {
			phase = "Pending"
		}
		strategy := m.Status.EffectiveStrategy
		if strategy == "" {
			strategy = m.Spec.Strategy
		}
		rep.Total++
		rep.ByPhase[phase]++
		rep.Entries = append(rep.Entries, auditEntry{
			Namespace: m.Metadata.Namespace, Name: m.Metadata.Name, Machine: m.Spec.MachineName,
			Created: m.Metadata.CreationTimestamp, Phase: phase, Strategy: strategy,
			Source: m.Status.SourceNode, Target: m.Status.TargetNode,
			DowntimeMs: m.Status.DowntimeMs, TotalTimeMs: m.Status.TotalTimeMs,
			Encrypted: m.Status.DataPlaneEncrypted, Message: m.Status.Message,
		})
	}
	sort.SliceStable(rep.Entries, func(i, j int) bool {
		if !rep.Entries[i].Created.Equal(rep.Entries[j].Created) {
			return rep.Entries[i].Created.After(rep.Entries[j].Created)
		}
		return rep.Entries[i].Name < rep.Entries[j].Name
	})
	if limit > 0 && len(rep.Entries) > limit {
		rep.Entries = rep.Entries[:limit]
	}
	return rep
}

func writeAudit(w io.Writer, rep auditReport, output string, now time.Time) error {
	switch output {
	case "json":
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	case "yaml":
		b, err := yaml.Marshal(rep)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "", "table":
	default:
		return fmt.Errorf("unknown output format %q (want table, json or yaml)", output)
	}
	if rep.Total == 0 {
		fmt.Fprintln(w, "no migrations in the selected window")
		return nil
	}
	phases := make([]string, 0, len(rep.ByPhase))
	for p := range rep.ByPhase {
		phases = append(phases, p)
	}
	sort.Strings(phases)
	parts := make([]string, 0, len(phases))
	for _, p := range phases {
		parts = append(parts, fmt.Sprintf("%s=%d", p, rep.ByPhase[p]))
	}
	fmt.Fprintf(w, "%d migration(s): %s\n\n", rep.Total, strings.Join(parts, " "))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGE\tNAMESPACE\tMACHINE\tSTRATEGY\tPHASE\tFROM\tTO\tDOWNTIME\tMESSAGE")
	for _, e := range rep.Entries {
		down := "-"
		if e.DowntimeMs > 0 {
			down = (time.Duration(e.DowntimeMs) * time.Millisecond).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			auditAge(now, e.Created), e.Namespace, e.Machine, dash(e.Strategy), e.Phase, dash(e.Source), dash(e.Target), down, dash(e.Message))
	}
	return tw.Flush()
}

func auditAge(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func runAudit(ctx context.Context, kc *kube.Client, out io.Writer, ns, machine string, since time.Duration, limit int, output string, now time.Time) error {
	var (
		migs []model.MachineMigration
		err  error
	)
	if ns == "" {
		migs, err = kc.ListMachineMigrations(ctx)
	} else {
		migs, err = kc.ListMachineMigrationsNamespace(ctx, ns)
	}
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	var cutoff time.Time
	if since > 0 {
		cutoff = now.Add(-since)
	}
	return writeAudit(out, buildAudit(migs, cutoff, machine, limit), output, now)
}

func newAuditCmd(opts *Options) *cobra.Command {
	var (
		all     bool
		machine string
		since   time.Duration
		limit   int
		output  string
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Summarize recent MachineMigrations (phase counts and a newest-first log)",
		Long: `Read-only summary of MachineMigration objects that still exist in the cluster:
how many reached each phase, then one line per migration, newest first, with
strategy, source and target node, downtime and the last status message.

It only reports what the API server still holds. Migrations that were deleted,
or garbage collected by your own cleanup, do not appear. Default scope is every
namespace; use -n to narrow it.`,
		Example: `  $ kaironctl audit --since 24h
  $ kaironctl audit -n prod --machine db-1
  $ kaironctl audit --since 7d -o json`,
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
			ns := ""
			if !all && cmd.Flags().Changed("namespace") {
				ns = opts.Namespace
			}
			return runAudit(ctx, kc, cmd.OutOrStdout(), ns, machine, since, limit, output, time.Now())
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "every namespace (the default unless -n is given)")
	cmd.Flags().StringVar(&machine, "machine", "", "only migrations of this Machine")
	cmd.Flags().DurationVar(&since, "since", 0, "only migrations created within this duration (for example 24h; 0 = all)")
	cmd.Flags().IntVar(&limit, "limit", 50, "most log lines to print (0 = all); counts always cover every match")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json|yaml")
	return cmd
}
