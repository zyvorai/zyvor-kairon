// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// maintenanceEnterPatch is the Node patch `node maintenance enter` sends.
func maintenanceEnterPatch(fallback, strategy string) (map[string]any, error) {
	var n model.Node
	n.Metadata.Annotations = map[string]string{
		model.AnnotationMaintenanceFallback: fallback,
		model.AnnotationMaintenanceStrategy: strategy,
	}
	if _, err := model.MaintenanceFallback(n); err != nil {
		return nil, err
	}
	if _, err := model.MaintenanceStrategy(n); err != nil {
		return nil, err
	}
	annotations := map[string]any{model.AnnotationMaintenance: "true"}
	for k, v := range map[string]string{model.AnnotationMaintenanceFallback: fallback, model.AnnotationMaintenanceStrategy: strategy} {
		if v == "" {
			annotations[k] = nil
		} else {
			annotations[k] = v
		}
	}
	return map[string]any{"metadata": map[string]any{"annotations": annotations}}, nil
}

func maintenanceExitPatch() map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		model.AnnotationMaintenance:         nil,
		model.AnnotationMaintenanceFallback: nil,
		model.AnnotationMaintenanceStrategy: nil,
	}}}
}

func newNodeMaintenanceCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "maintenance",
		Short: "Put a node into or out of maintenance mode",
		Long: `Maintenance mode is driven by kairon-controller (-node-maintenance, on by
default). Entering annotates the Node with kairon.zyvor.dev/maintenance=true;
the controller cordons it, migrates its Machines to other nodes and, for
Machines no other node can take, applies --fallback: wait leaves them
running, stop powers them off, hibernate saves their state (qemu backend,
others are stopped). Exiting removes the annotation; the controller
uncordons the node if it cordoned it and restores every Machine it parked.`,
	}
	var fallback, strategy string
	var wait bool
	var timeout, poll time.Duration
	enter := &cobra.Command{
		Use:   "enter NODE",
		Short: "Start maintenance: cordon NODE and move its Machines away",
		Example: `  kaironctl node maintenance enter worker-2
  kaironctl node maintenance enter worker-2 --fallback stop --wait --timeout 30m`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch, err := maintenanceEnterPatch(fallback, strategy)
			if err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if err := kc.PatchNode(cmd.Context(), args[0], patch); err != nil {
				return err
			}
			okf("node/%s: maintenance requested", args[0])
			if !wait {
				return nil
			}
			deadline := time.Now().Add(timeout)
			for {
				n, err := kc.GetNode(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				a := n.Metadata.Annotations
				fmt.Printf("%s: %s: %s\n", time.Now().UTC().Format(time.RFC3339), valueOr(a[model.AnnotationMaintenanceState], "Pending"), a[model.AnnotationMaintenanceMessage])
				if a[model.AnnotationMaintenanceState] == model.MaintenanceStateReady {
					okf("node/%s: in maintenance", args[0])
					return nil
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("node/%s did not reach %s within %s", args[0], model.MaintenanceStateReady, timeout)
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(poll):
				}
			}
		},
	}
	enter.Flags().StringVar(&fallback, "fallback", "", "what to do with a Machine no other node can take: wait|stop|hibernate (default wait)")
	enter.Flags().StringVar(&strategy, "strategy", "", "migration strategy: live|auto|cold (default live; Machines live migration refuses move cold)")
	enter.Flags().BoolVar(&wait, "wait", false, "wait until the node reports InMaintenance")
	enter.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "with --wait, how long to wait")
	enter.Flags().DurationVar(&poll, "poll-interval", 5*time.Second, "with --wait, how often to check")

	exit := &cobra.Command{
		Use:     "exit NODE",
		Short:   "End maintenance: uncordon NODE and restore parked Machines",
		Example: `  kaironctl node maintenance exit worker-2`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			if err := kc.PatchNode(cmd.Context(), args[0], maintenanceExitPatch()); err != nil {
				return err
			}
			okf("node/%s: maintenance exit requested", args[0])
			return nil
		},
	}

	status := &cobra.Command{
		Use:     "status [NODE]",
		Short:   "Show maintenance state of every node, or of NODE",
		Example: `  kaironctl node maintenance status`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			nodes, err := kc.ListNodes(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) == 1 {
				var only []model.Node
				for _, n := range nodes {
					if n.Metadata.Name == args[0] {
						only = append(only, n)
					}
				}
				if len(only) == 0 {
					return fmt.Errorf("node %q not found", args[0])
				}
				nodes = only
			}
			printMaintenanceStatus(os.Stdout, nodes)
			return nil
		},
	}
	root.AddCommand(enter, exit, status)
	return root
}

func printMaintenanceStatus(w io.Writer, nodes []model.Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Metadata.Name < nodes[j].Metadata.Name })
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tMAINTENANCE\tSTATE\tSCHEDULABLE\tMESSAGE")
	for _, n := range nodes {
		a := n.Metadata.Annotations
		requested := "no"
		if model.MaintenanceRequested(n) {
			requested = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", n.Metadata.Name, requested, valueOr(a[model.AnnotationMaintenanceState], "-"), !n.Spec.Unschedulable, valueOr(a[model.AnnotationMaintenanceMessage], "-"))
	}
	_ = tw.Flush()
}

func valueOr(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func newDRSCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "drs",
		Short: "Inspect DRS placement rebalancing",
	}
	var cfg controller.DRS
	var output string
	plan := &cobra.Command{
		Use:   "plan",
		Short: "Show the moves DRS would make now",
		Long: `Runs the same planner kairon-controller's -drs-mode uses against the
cluster's current Machines and Nodes, and prints each node's reserved
utilization and the moves that would lower the imbalance. Nothing is
changed. Flags mirror the controller's -drs-* flags.`,
		Example: `  kaironctl drs plan
  kaironctl drs plan --threshold-percent 10 --max-moves 5 -o json
  kaironctl drs plan --signal pressure --pressure-threshold-percent 15`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := controller.ValidateDRSSignal(cfg.Signal); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			p, err := drsPlan(cmd.Context(), kc, cfg)
			if err != nil {
				return err
			}
			if output == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(p)
			}
			printDRSPlan(os.Stdout, p)
			return nil
		},
	}
	plan.Flags().IntVar(&cfg.ThresholdPercent, "threshold-percent", scheduler.DefaultRebalanceThresholdPercent, "imbalance at which moves are planned")
	plan.Flags().IntVar(&cfg.MinImprovementPercent, "min-improvement-percent", scheduler.DefaultRebalanceMinImprovementPercent, "how much a single move must lower the imbalance")
	plan.Flags().IntVar(&cfg.MaxMoves, "max-moves", scheduler.DefaultRebalanceMaxMoves, "most moves to plan")
	plan.Flags().StringVar(&cfg.Strategy, "strategy", "live", "live|auto|cold; with live only Machines live migration can move are considered")
	plan.Flags().DurationVar(&cfg.Cooldown, "cooldown", controller.DefaultDRSCooldown, "skip Machines DRS moved more recently than this")
	plan.Flags().StringVar(&cfg.Signal, "signal", scheduler.RebalanceSignalReserved, "reserved|pressure: balance reserved CPU/memory, or move Machines off nodes with high measured PSI")
	plan.Flags().IntVar(&cfg.PressureThresholdPercent, "pressure-threshold-percent", scheduler.DefaultPressureThresholdPercent, "with --signal pressure, host PSI at which a node is hot")
	plan.Flags().DurationVar(&cfg.PressureMaxAge, "pressure-max-age", scheduler.DefaultPressureMaxAge, "with --signal pressure, ignore nodes whose sample is older than this")
	plan.Flags().StringVarP(&output, "output", "o", "", "output format: json")
	root.AddCommand(plan)
	return root
}

type drsLister interface {
	ListMachines(ctx context.Context) ([]model.Machine, error)
	ListNodes(ctx context.Context) ([]model.Node, error)
	ListMachineMigrations(ctx context.Context) ([]model.MachineMigration, error)
}

func drsPlan(ctx context.Context, kc drsLister, cfg controller.DRS) (scheduler.RebalancePlan, error) {
	machines, err := kc.ListMachines(ctx)
	if err != nil {
		return scheduler.RebalancePlan{}, err
	}
	nodes, err := kc.ListNodes(ctx)
	if err != nil {
		return scheduler.RebalancePlan{}, err
	}
	migrations, err := kc.ListMachineMigrations(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return scheduler.RebalancePlan{}, err
	}
	busy := map[string]bool{}
	for _, mig := range migrations {
		if migrationStillPending(mig.Status.Phase) {
			busy[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}
	s := scheduler.Scheduler{RequireCapableLabel: true}
	return s.PlanRebalance(nodes, machines, cfg.RebalanceOptions(busy, time.Now().UTC())), nil
}

func printDRSPlan(w io.Writer, p scheduler.RebalancePlan) {
	if p.Signal == scheduler.RebalanceSignalPressure {
		printDRSPressurePlan(w, p)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tCPU%\tMEMORY%\tUTILIZATION%\tMACHINES")
	for _, u := range p.Nodes {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\n", u.Node, u.CPUPercent, u.MemoryPercent, u.Percent, u.Machines)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nimbalance: %d%%", p.ImbalanceBefore)
	if len(p.Moves) > 0 {
		_, _ = fmt.Fprintf(w, " -> %d%% after %d move(s)\n\n", p.ImbalanceAfter, len(p.Moves))
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "MACHINE\tFROM\tTO\tIMBALANCE AFTER")
		for _, mv := range p.Moves {
			_, _ = fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%d%%\n", mv.Namespace, mv.Machine, mv.From, mv.To, mv.Imbalance)
		}
		_ = tw.Flush()
		return
	}
	_, _ = fmt.Fprintf(w, "\nno moves: %s\n", p.Reason)
}

// printDRSPressurePlan prints a -signal=pressure plan: each node's host PSI
// (some avg60) next to its reserved utilization, and why each move is
// planned.
func printDRSPressurePlan(w io.Writer, p scheduler.RebalancePlan) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tCPU PSI%\tMEMORY PSI%\tIO PSI%\tUTILIZATION%\tMACHINES")
	for _, u := range p.Nodes {
		if u.Pressure == nil {
			_, _ = fmt.Fprintf(tw, "%s\t-\t-\t-\t%d\t%d\n", u.Node, u.Percent, u.Machines)
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%.1f\t%d\t%d\n", u.Node, u.Pressure.CPU, u.Pressure.Memory, u.Pressure.IO, u.Percent, u.Machines)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\npressure spread: %d points\n", p.ImbalanceBefore)
	if len(p.Moves) == 0 {
		_, _ = fmt.Fprintf(w, "\nno moves: %s\n", p.Reason)
		return
	}
	_, _ = fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MACHINE\tFROM\tTO\tWHY")
	for _, mv := range p.Moves {
		_, _ = fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\n", mv.Namespace, mv.Machine, mv.From, mv.To, mv.Reason)
	}
	_ = tw.Flush()
}
