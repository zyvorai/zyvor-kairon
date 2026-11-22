// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdTop(ctx context.Context, kc *kube.Client, args []string) {
	format, args, ferr := extractOutput(args)
	if ferr != nil {
		fatal(ferr)
	}
	if format == "name" {
		fatal(fmt.Errorf("top supports -o json or -o yaml"))
	}
	ns, args := nsFlag(args)
	resource := "machines"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		resource = strings.ToLower(args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	var selectorFlag stringSliceFlag
	fs.Var(&selectorFlag, "selector", "label key=value every counted Machine must carry (repeatable -- every pair must match); omitted counts every Machine, exactly as `kaironctl get`'s own --selector does")
	_ = fs.Parse(args)
	selector, err := parseKeyValues(selectorFlag)
	if err != nil {
		fatal(err)
	}
	switch resource {
	case "machine", "machines", "vm", "vms":
		items, err := kc.ListMachinesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(m model.Machine) map[string]string { return m.Metadata.Labels })
		sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
		if format != "" {
			type row struct {
				Name           string   `json:"name"`
				Node           string   `json:"node,omitempty"`
				CPUPercent     *float64 `json:"cpuPercent,omitempty"`
				MemoryBytes    *uint64  `json:"memoryBytes,omitempty"`
				DiskReadBytes  *uint64  `json:"diskReadBytes,omitempty"`
				DiskWriteBytes *uint64  `json:"diskWriteBytes,omitempty"`
			}
			rows := make([]row, 0, len(items))
			for _, m := range items {
				r := row{Name: m.Metadata.Name, Node: m.Spec.NodeName}
				if u := m.Status.ResourceUsage; u != nil {
					cpu, mem, rd, wr := u.CPUPercent, u.MemoryBytes, u.DiskReadBytes, u.DiskWriteBytes
					r.CPUPercent, r.MemoryBytes, r.DiskReadBytes, r.DiskWriteBytes = &cpu, &mem, &rd, &wr
				}
				rows = append(rows, r)
			}
			if err := writeStructured(os.Stdout, rows, format); err != nil {
				fatal(err)
			}
			return
		}
		fmt.Printf("NAME\tNODE\tCPU%%\tMEMORY\tDISK-READ\tDISK-WRITE\n")
		for _, m := range items {
			u := m.Status.ResourceUsage
			if u == nil {
				fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", m.Metadata.Name, dash(m.Spec.NodeName), "-", "-", "-", "-")
				continue
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", m.Metadata.Name, dash(m.Spec.NodeName), formatCPUPercent(u.CPUPercent), formatBytes(u.MemoryBytes), formatBytes(u.DiskReadBytes), formatBytes(u.DiskWriteBytes))
		}
	case "node", "nodes":
		// Cluster-wide by construction -- see this function's own doc
		// comment for why ns is unused here.
		items, err := kc.ListMachines(ctx)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(m model.Machine) map[string]string { return m.Metadata.Labels })
		if format != "" {
			if err := writeStructured(os.Stdout, model.AggregateUsageByNode(items), format); err != nil {
				fatal(err)
			}
			return
		}
		fmt.Printf("NODE\tMACHINES\tCPU%%\tMEMORY\n")
		for _, agg := range model.AggregateUsageByNode(items) {
			fmt.Printf("%s\t%d\t%s\t%s\n", agg.Node, agg.Machines, formatCPUPercent(agg.CPUPercent), formatBytes(agg.MemoryBytes))
		}
	default:
		fatal(fmt.Errorf("unknown resource %q", resource))
	}
}

// The node-grouping/summing behind `kaironctl top nodes` itself now lives
// in model.AggregateUsageByNode (internal/model/usage.go) rather than
// here: kairon-ui's Nodes dashboard page needs the exact same rollup
// (GET /api/v1/nodes/usage, internal/uiapi's handleNodeUsage) and a
// second, independently-maintained copy of this grouping/summing logic
// in internal/uiapi would be exactly the kind of two-implementations-of-
// one-rule drift this codebase avoids elsewhere (see e.g.
// model.LabelsMatch, shared the same way between the scheduler and every
// selector-filtering CLI verb).

// formatCPUPercent renders ResourceUsage.CPUPercent to two decimal places
// with a trailing "%" -- kubectl top's own "123m"-style millicore notation
// doesn't apply here (CPUPercent is already a percentage of one core, per
// its own doc comment, not a raw core-second count), so this is simply a
// fixed, readable precision rather than Go's default float formatting
// (which would print a trailing ".000000000001"-style artifact for values
// like agent.go's own averaged-over-lifetime computation can produce).
func formatCPUPercent(p float64) string {
	return strconv.FormatFloat(p, 'f', 2, 64) + "%"
}

// formatBytes renders a byte count the same human-readable way `kaironctl`
// already expects an operator reading a terminal to want (MachineQuota's
// own MaxTotalMemory/MaxTotalMemoryMiB fields are hand-authored strings for
// exactly this reason) -- ResourceUsage's fields are raw uint64 byte
// counts straight off FluxVM's stats endpoint, and printing those
// unformatted would force every reader to mentally divide by 2^30 anyway.
// Binary (1024-based) units, matching Kubernetes' own Mi/Gi convention
// (never SI Mb/Gb) that MachineQuota/MachineInstanceType's own Memory
// fields already use throughout this codebase.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// nodeReadyStatus renders a Node's core "Ready" NodeCondition for
// `kaironctl get nodes` -- the same condition kairon-controller's own
// ConditionNodeUnreachable/fencing logic (see cmdFence) ultimately treats
// as the node's reachability signal. "Unknown" covers both a genuinely
// absent Ready condition and one whose Status is neither "True" nor
// "False", since a real kube-apiserver only ever reports those three
// values for it anyway.
func nodeReadyStatus(n model.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status
		}
	}
	return "Unknown"
}

// nodeTaintsSummary renders a Node's spec.taints as a comma-separated
// key[=value]:Effect list ("-" when there are none) for `kaironctl get
// nodes`/`describe node`'s own raw dump -- the same key[=value] shorthand
// internal/scheduler.go's own unexported taintKV renders for its
// excluded-node reason strings, reimplemented here (rather than exported
// from internal/scheduler just for this) since it's a two-line, purely
// cosmetic formatting helper with no shared behavior that could drift.
func nodeTaintsSummary(n model.Node) string {
	if len(n.Spec.Taints) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(n.Spec.Taints))
	for _, t := range n.Spec.Taints {
		kv := t.Key
		if t.Value != "" {
			kv += "=" + t.Value
		}
		parts = append(parts, kv+":"+t.Effect)
	}
	return strings.Join(parts, ",")
}

// defaultStrategy names the MachineSet rollout strategy the reconciler
// itself defaults to when spec.strategy is left empty, so `kaironctl get
// machinesets` never prints a bare "-" for the common case.
func defaultStrategy(strategy string) string {
	if strategy == "" {
		return "RollingUpdate"
	}
	return strategy
}

// formatNextRun renders `kaironctl get snapshotschedules`' NEXTRUN column.
// It deliberately does NOT just print s.Status.NextRunTime verbatim: that
// field is only ever updated when a schedule actually fires (see
// MachineSnapshotScheduleStatus.NextRunTime's own doc comment), so a
// schedule suspended sometime *after* its last run would otherwise show an
// already-passed timestamp as if a run were still pending. Checking the
// live spec.suspend flag here, at display time, means this can never
// happen -- "suspended" always wins over a stale projection.
func formatNextRun(s model.MachineSnapshotSchedule) string {
	if s.Spec.Suspend {
		return "suspended"
	}
	if s.Status.NextRunTime.IsZero() {
		return "pending" // never yet run -- fires on the next reconcile tick
	}
	return s.Status.NextRunTime.Format(time.RFC3339)
}

// resourceKindAndName splits describe/delete's positional args into a
// resource kind and a NAME, recognizing exactly the same resource-type
// aliases cmdGet already does -- reused here so all three verbs treat
// "snapshot"/"machineset"/etc. identically. KIND is always optional and
// disambiguated purely by argument count, never by matching NAME itself
// against the alias list: 1 positional arg is NAME alone (kind defaults
// to "machine", preserving the exact prior calling convention of
// `describe NAME`/`delete NAME`), 2 positional args are KIND NAME. This
// avoids the alternative of guessing from NAME's own text, which would
// silently misfire for the (rare but real) case of a Machine actually
