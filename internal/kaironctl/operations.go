// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/changeplan"
	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/incidentwork"
	"github.com/zyvorai/kairon/internal/llm"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/rehearsal"
	"github.com/zyvorai/kairon/internal/scheduler"
)

func readOperationJSON(file string, out any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON document")
	}
	return nil
}
func reserveOperationReport(file string) error {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

func newChangePlanCmd() *cobra.Command {
	root := &cobra.Command{Use: "changeplan", Short: "Preview, apply and recover health-gated configuration transactions (experimental)"}
	for _, action := range []string{"preview", "apply", "recover"} {
		var file, output string
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs, Short: action + " a versioned change plan", RunE: func(cmd *cobra.Command, _ []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			e := changeplan.Engine{Kube: kc, Exec: rehearsalExec, Save: func(r changeplan.Report) error { return rehearsal.WriteReport(output, r) }}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			if action == "recover" {
				var report changeplan.Report
				if err := readOperationJSON(file, &report); err != nil {
					return err
				}
				e.Save = func(r changeplan.Report) error { return rehearsal.WriteReport(file, r) }
				recoveryCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
				defer stop()
				err := e.Recover(recoveryCtx, &report)
				if writeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err == nil {
					err = writeErr
				}
				return err
			}
			var plan changeplan.Plan
			if err := readOperationJSON(file, &plan); err != nil {
				return err
			}
			if action == "preview" {
				plan, err = e.Prepare(ctx, plan)
				if err != nil {
					return err
				}
				if output != "" {
					if err := reserveOperationReport(output); err != nil {
						return err
					}
					return rehearsal.WriteReport(output, plan)
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(plan)
			}
			if output == "" {
				return fmt.Errorf("--output is required for apply's durable journal")
			}
			if uiBaseURL() == "" {
				return fmt.Errorf("KAIRON_UI_URL is required for health gates")
			}
			if err := plan.Validate(true); err != nil {
				return err
			}
			if err := reserveOperationReport(output); err != nil {
				return err
			}
			report, err := e.Apply(ctx, plan)
			_, printErr := fmt.Fprintf(cmd.OutOrStdout(), "changeplan %s: %s; journal=%s\n", report.ID, report.Phase, output)
			if err != nil {
				return err
			}
			return printErr
		}}
		cmd.Flags().StringVar(&file, "file", "", "input plan, prepared plan, or recovery journal")
		cmd.Flags().StringVar(&output, "output", "", "new prepared-plan or journal file")
		_ = cmd.MarkFlagRequired("file")
		root.AddCommand(cmd)
	}
	return root
}

func newRecoveryVerificationCmd() *cobra.Command {
	root := &cobra.Command{Use: "recovery", Short: "Verify application recovery from recorded CSI MachineSnapshots (experimental)"}
	root.AddCommand(newOperationalRehearsal("verify", "recovery"))
	return root
}
func newIncidentWorkflowCmd() *cobra.Command {
	root := &cobra.Command{Use: "incident", Short: "Reproduce an incident on isolated copies and test a proposed fix (experimental)"}
	root.AddCommand(newOperationalRehearsal("rehearse", "incident"))
	return root
}
func newOperationalRehearsal(verb, purpose string) *cobra.Command {
	var file, reportFile, bundle string
	var ai bool
	cmd := &cobra.Command{Use: verb, Args: cobra.NoArgs, Short: "Run an isolated " + purpose + " application workflow", RunE: func(cmd *cobra.Command, _ []string) error {
		var p rehearsal.Plan
		if err := readOperationJSON(file, &p); err != nil {
			return err
		}
		if p.Purpose != "" && p.Purpose != purpose {
			return fmt.Errorf("plan purpose does not match this command")
		}
		p.Purpose = purpose
		if purpose == "incident" {
			if bundle == "" {
				return fmt.Errorf("--bundle is required")
			}
			f, err := os.Open(bundle)
			if err != nil {
				return err
			}
			p, err = incidentwork.Bind(f, p)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		if err := p.Validate(); err != nil {
			return err
		}
		if uiBaseURL() == "" {
			return fmt.Errorf("KAIRON_UI_URL is required")
		}
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		if err := reserveOperationReport(reportFile); err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		r := rehearsal.Runner{Kube: kc, Exec: rehearsalExec, Save: func(out rehearsal.Report) error { return rehearsal.WriteReport(reportFile, out) }}
		if purpose == "incident" {
			r.Investigate = func(ctx context.Context, ns, name string) (json.RawMessage, error) {
				d, err := diagnoseRef(ctx, kc, ns, "machine/"+name, false)
				if err != nil {
					return nil, err
				}
				d.Healthy = false
				d.Summary = "An application failure was reproduced on this isolated copy; runtime readiness is not application health."
				cause := agentplane.Cause{Title: "Application failure reproduced", Evidence: "The declared incident baseline returned its expected nonzero exit status.", Propose: []string{"Review the declared fix steps against the incident recovery point before production use.", "Inspect application schema, configuration and dependency compatibility on the isolated copy."}}
				d.Causes = append([]agentplane.Cause{cause}, d.Causes...)
				for i := range d.Causes {
					d.Causes[i].Rank = i + 1
				}
				if ai {
					client := llm.FromEnv()
					if client == nil {
						return nil, llm.ErrNotConfigured
					}
					d, err = agentplane.Summarize(ctx, client, d)
					if err != nil {
						return nil, err
					}
				}
				return json.Marshal(d)
			}
		}
		out, err := r.Run(ctx, p)
		_, printErr := fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s; cleaned=%t; report=%s\n", purpose, out.RunID, out.Phase, out.Cleaned, reportFile)
		if err != nil {
			return err
		}
		return printErr
	}}
	cmd.Flags().StringVar(&file, "file", "", "JSON workflow plan")
	cmd.Flags().StringVar(&reportFile, "report", "", "new private report/journal file")
	_ = cmd.MarkFlagRequired("file")
	_ = cmd.MarkFlagRequired("report")
	if purpose == "incident" {
		cmd.Flags().StringVar(&bundle, "bundle", "", "redacted Kairon incident tar.gz (data only)")
		cmd.Flags().BoolVar(&ai, "ai", false, "optionally summarize copy diagnostics through the configured model; never applies model output")
	}
	return cmd
}

func newNetworkPlacementCmd() *cobra.Command {
	var telemetryFile string
	var options scheduler.NetworkPlacementOptions
	root := &cobra.Command{Use: "placement", Short: "Traffic-weighted placement recommendations (experimental; no migrations applied)"}
	recommend := &cobra.Command{Use: "recommend", Args: cobra.NoArgs, Short: "Combine measured VM traffic/RTTs with live capacity, constraints and host pressure", RunE: func(cmd *cobra.Command, _ []string) error {
		var telemetry scheduler.NetworkTelemetry
		if err := readOperationJSON(telemetryFile, &telemetry); err != nil {
			return err
		}
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		nodes, err := kc.ListNodes(cmd.Context())
		if err != nil {
			return err
		}
		machines, err := kc.ListMachines(cmd.Context())
		if err != nil {
			return err
		}
		migrations, err := kc.ListMachineMigrations(cmd.Context())
		if err != nil {
			return err
		}
		options.Busy = map[string]bool{}
		for _, m := range migrations {
			if m.Status.Phase != "Succeeded" && m.Status.Phase != "Failed" && m.Status.Phase != "Cancelled" {
				options.Busy[m.Namespace()+"/"+m.Spec.MachineName] = true
			}
		}
		report, err := (scheduler.Scheduler{RequireCapableLabel: true}).PlanNetworkPlacement(nodes, machines, telemetry, options, time.Now().UTC())
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}}
	recommend.Flags().StringVar(&telemetryFile, "telemetry", "", "measured VM traffic graph and node RTT JSON")
	recommend.Flags().IntVar(&options.MaxMoves, "max-moves", 1, "maximum recommendations")
	recommend.Flags().Float64Var(&options.MinImprovementPercent, "min-improvement", 10, "minimum estimated RTT-cost improvement percent")
	recommend.Flags().Float64Var(&options.MaxPressurePercent, "max-pressure", 25, "maximum measured host PSI percent")
	recommend.Flags().IntVar(&options.MaxAgeSeconds, "max-age", 120, "maximum telemetry and pressure age in seconds")
	recommend.Flags().IntVar(&options.CooldownSeconds, "cooldown", 3600, "seconds since the last DRS move")
	recommend.Flags().Float64Var(&options.MigrationPenalty, "migration-penalty", 0, "minimum saved byte-ms/s above migration cost")
	_ = recommend.MarkFlagRequired("telemetry")
	root.AddCommand(recommend, newPlacementCollectCmd())
	return root
}

func newPlacementCollectCmd() *cobra.Command {
	var rtts, output string
	var window time.Duration
	cmd := &cobra.Command{Use: "collect", Args: cobra.NoArgs, Short: "Measure cumulative VM egress counters over a window and combine with independently measured node RTTs", RunE: func(cmd *cobra.Command, _ []string) error {
		if window < time.Second || window > 30*time.Second {
			return fmt.Errorf("window must be 1..30 seconds")
		}
		if uiBaseURL() == "" {
			return fmt.Errorf("KAIRON_UI_URL is required")
		}
		var telemetry scheduler.NetworkTelemetry
		if err := readOperationJSON(rtts, &telemetry); err != nil {
			return err
		}
		kc, err := newKubeClient()
		if err != nil {
			return err
		}
		machines, err := kc.ListMachines(cmd.Context())
		if err != nil {
			return err
		}
		type sample struct {
			Flows []ebpfedge.FlowRecord
			At    time.Time
		}
		snapshot := func() (map[string]sample, error) {
			result := map[string]sample{}
			for _, m := range machines {
				if m.Status.Phase != "Running" || m.Status.GuestIP == "" {
					continue
				}
				raw, err := uiRaw(cmd.Context(), http.MethodGet, machinePath(m.Namespace(), m.Metadata.Name, "network-flows")+"?limit=10000", nil)
				if err != nil {
					return nil, err
				}
				flows, err := scheduler.DecodeMeasuredFlows(raw)
				if err != nil {
					return nil, err
				}
				result[m.Namespace()+"/"+m.Metadata.Name] = sample{Flows: flows, At: time.Now().UTC()}
			}
			return result, nil
		}
		before, err := snapshot()
		if err != nil {
			return err
		}
		start := time.Now().UTC()
		timer := time.NewTimer(window)
		defer timer.Stop()
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-timer.C:
		}
		after, err := snapshot()
		if err != nil {
			return err
		}
		end := time.Now().UTC()
		current, err := kc.ListMachines(cmd.Context())
		if err != nil {
			return err
		}
		identity := map[string]model.Machine{}
		for _, m := range current {
			identity[m.Namespace()+"/"+m.Metadata.Name] = m
		}
		for _, m := range machines {
			id := m.Namespace() + "/" + m.Metadata.Name
			n := identity[id]
			if m.Metadata.UID != n.Metadata.UID || m.Status.GuestIP != n.Status.GuestIP || !reflect.DeepEqual(m.Status.GuestIPs, n.Status.GuestIPs) || m.Spec.NodeName != n.Spec.NodeName || m.Status.RuntimeID != n.Status.RuntimeID {
				return fmt.Errorf("machine identities, addresses or locations changed during collection")
			}
		}
		var edges []scheduler.TrafficEdge
		var stats scheduler.FlowCollectionStats
		for id, a := range after {
			b, ok := before[id]
			if !ok {
				return fmt.Errorf("missing baseline flow sample")
			}
			measured, skipped, err := scheduler.TrafficFromCounters(machines, map[string][]ebpfedge.FlowRecord{id: b.Flows}, map[string][]ebpfedge.FlowRecord{id: a.Flows}, b.At, a.At)
			if err != nil {
				return err
			}
			edges = append(edges, measured...)
			stats.External += skipped.External
			stats.Denied += skipped.Denied
			stats.Reset += skipped.Reset
			stats.Unbounded += skipped.Unbounded
		}
		if len(edges) == 0 {
			return fmt.Errorf("no unambiguous inter-VM egress traffic observed; skipped=%+v", stats)
		}
		telemetry.APIVersion = "networkplacement.kairon.zyvor.dev/v1alpha1"
		telemetry.CapturedAt = end
		telemetry.WindowSeconds = int(math.Ceil(end.Sub(start).Seconds()))
		telemetry.Edges = edges
		if err := reserveOperationReport(output); err != nil {
			return err
		}
		if err := rehearsal.WriteReport(output, telemetry); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
	}}
	cmd.Flags().StringVar(&rtts, "rtts", "", "JSON template containing actual node-pair RTT measurements and measured same-node RTT")
	cmd.Flags().StringVar(&output, "output", "", "new telemetry JSON output")
	cmd.Flags().DurationVar(&window, "window", 10*time.Second, "cumulative-counter sampling window, 1..30s")
	_ = cmd.MarkFlagRequired("rtts")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}
