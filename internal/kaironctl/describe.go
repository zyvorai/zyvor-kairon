// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func resourceKindAndName(verb string, args []string) (kind, name string) {
	switch len(args) {
	case 1:
		return "machine", args[0]
	case 2:
		return strings.ToLower(args[0]), args[1]
	default:
		fatal(fmt.Errorf("usage: kaironctl %s [RESOURCE] NAME", verb))
		return "", ""
	}
}

func cmdDescribe(ctx context.Context, kc *kube.Client, args []string) {
	format, args, ferr := extractOutput(args)
	if ferr != nil {
		fatal(ferr)
	}
	ns, args := nsFlag(args)
	kind, name := resourceKindAndName("describe", args)
	if format != "" {
		if err := describeStructured(ctx, kc, os.Stdout, kind, ns, name, format); err != nil {
			fatal(err)
		}
		return
	}
	switch kind {
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		// One of four kinds describe doesn't just raw-JSON-dump -- see
		// describeSnapshotSchedule's own doc comment for why this
		// narrow exception is justified for this specific CRD and isn't
		// a generalized richer-describe change for every kind.
		describeSnapshotSchedule(ctx, kc, ns, name)
		return
	case "migrationpolicy", "migrationpolicies":
		// The second -- see describeMigrationPolicy's own doc comment.
		describeMigrationPolicy(ctx, kc, ns, name)
		return
	case "quota", "quotas", "machinequotas":
		// The third -- see describeQuota's own doc comment.
		describeQuota(ctx, kc, ns, name)
		return
	case "budget", "budgets", "machinedisruptionbudgets":
		// The fourth -- see describeBudget's own doc comment.
		describeBudget(ctx, kc, ns, name)
		return
	case "machine", "machines", "vm", "vms":
		// The fifth -- see describeMachine's own doc comment.
		describeMachine(ctx, kc, ns, name)
		return
	case "migration", "migrations", "machinemigrations":
		describeMigration(ctx, kc, ns, name)
		return
	case "backuppolicy", "backuppolicies", "machinebackuppolicies":
		describeBackupPolicy(ctx, kc, ns, name)
		return
	case "virtualnetwork", "virtualnetworks", "vnet", "vnets":
		describeVirtualNetwork(ctx, kc, ns, name)
		return
	case "backup", "backups", "machinebackups":
		describeBackup(ctx, kc, ns, name)
		return
	}
	var (
		out any
		err error
	)
	switch kind {
	case "machine", "machines", "vm", "vms":
		out, err = kc.GetMachine(ctx, ns, name)
	case "migration", "migrations", "machinemigrations":
		out, err = kc.GetMachineMigration(ctx, ns, name)
	case "snapshot", "snapshots", "machinesnapshots":
		out, err = kc.GetMachineSnapshot(ctx, ns, name)
	case "restore", "restores", "machinesnapshotrestores":
		out, err = kc.GetMachineSnapshotRestore(ctx, ns, name)
	case "backup", "backups", "machinebackups":
		out, err = kc.GetMachineBackup(ctx, ns, name)
	case "backuprestore", "backuprestores", "machinebackuprestores":
		out, err = kc.GetMachineBackupRestore(ctx, ns, name)
	case "quota", "quotas", "machinequotas":
		out, err = kc.GetMachineQuota(ctx, ns, name)
	case "budget", "budgets", "machinedisruptionbudgets":
		out, err = kc.GetMachineDisruptionBudget(ctx, ns, name)
	case "machineset", "machinesets":
		out, err = kc.GetMachineSet(ctx, ns, name)
	case "instancetype", "instancetypes", "machineinstancetypes":
		out, err = kc.GetMachineInstanceType(ctx, ns, name)
	case "migrationpolicy", "migrationpolicies":
		out, err = kc.GetMigrationPolicy(ctx, ns, name)
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		out, err = kc.GetMachineNetworkPolicy(ctx, ns, name)
	case "securitygroup", "securitygroups", "networksecuritygroups":
		out, err = kc.GetNetworkSecurityGroup(ctx, ns, name)
	case "node", "nodes":
		// Cluster-scoped, like `kaironctl get node` above -- ns is unused.
		out, err = kc.GetNode(ctx, name)
	case "nodetopology", "nodetopologies", "knt":
		out, err = kc.GetNodeTopology(ctx, name)
	default:
		fatal(fmt.Errorf("unknown resource %q", kind))
		return
	}
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// describeSnapshotSchedule prints a MachineSnapshotSchedule the same
// raw-JSON way every other kind's `describe` does, then appends a
// "Matching machines" preview: exactly which Machines the schedule's own
// spec.selector currently matches in its namespace, and whether the very
// next reconcile tick would actually fire a new round of snapshots for
// them -- evaluated by calling the schedule's own Spec.Due(status.
// lastRunTime, time.Now()) and, if Due, Spec.DeadlineExceeded(status.
// lastRunTime, time.Now()) -- the identical pure functions
// reconcileMachineSnapshotSchedules itself calls every tick, so this
// preview can never drift from what the controller will actually do. A
// schedule that's Due but past its own spec.startingDeadlineSeconds is
// reported as a distinct third outcome ("due, but will be SKIPPED") rather
// than folded into either "due now" or "not due yet" -- it fires no
// snapshots this tick, same as "not due", but for a different, worth-
// surfacing reason.
//
// This is a deliberate, narrow exception to this project's otherwise
// uniform "describe just dumps the raw object as JSON" convention (every
// other kind still does exactly that, unchanged -- describeMigrationPolicy
// below is the only other exception) -- justified because kubectl's own
// `describe` already appends non-raw derived information beyond an
// object's literal fields when it's operationally useful (e.g. related
// Events), and "which Machines would this schedule snapshot right now, and
// would it even fire" is a real question an operator asks before
// loosening/tightening spec.selector or spec.intervalSeconds, not
// something a generalized richer-describe-for-every-kind change would be
// needed for.
func describeSnapshotSchedule(ctx context.Context, kc *kube.Client, ns, name string) {
	sched, err := kc.GetMachineSnapshotSchedule(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(sched, "", "  ")
	fmt.Println(string(b))

	machines, err := kc.ListMachinesNamespace(ctx, sched.Namespace())
	if err != nil {
		fatal(err)
	}
	var matches []string
	for _, m := range machines {
		if model.LabelsMatch(m.Metadata.Labels, sched.Spec.Selector) {
			matches = append(matches, m.Metadata.Name)
		}
	}
	sort.Strings(matches)

	fmt.Println()
	now := time.Now()
	switch {
	case model.TriggerNowRequested(sched.Metadata.Annotations[model.AnnotationSnapshotScheduleTriggerNow], sched.Status.LastHandledTriggerTime):
		fmt.Printf("Matching machines (%d) -- manual run requested (kaironctl trigger snapshotschedule), the next reconcile tick will snapshot these regardless of spec.suspend or the normal interval:\n", len(matches))
	case sched.Due(sched.Status.LastRunTime, now) && sched.DeadlineExceeded(sched.Status.LastRunTime, now):
		fmt.Printf("Matching machines (%d) -- due, but will be SKIPPED: this run is more than startingDeadlineSeconds (%ds) late:\n", len(matches), sched.Spec.StartingDeadlineSeconds)
	case sched.Due(sched.Status.LastRunTime, now):
		fmt.Printf("Matching machines (%d) -- due now, the next reconcile tick will snapshot these:\n", len(matches))
	default:
		fmt.Printf("Matching machines (%d) -- not due yet (next projected run: %s):\n", len(matches), formatNextRun(sched))
	}
	if len(matches) == 0 {
		fmt.Println("  (none -- check spec.selector against these Machines' own labels)")
		return
	}
	for _, m := range matches {
		fmt.Printf("  %s\n", m)
	}
}

// describeMigrationPolicy is `describe`'s other raw-JSON-plus-preview
// exception (see describeSnapshotSchedule's doc comment just above for why
// this is deliberately narrow, not a generalized richer-describe change):
// after the usual raw-JSON dump, it appends exactly which Machines in the
// policy's own namespace currently match spec.selector, and for each one,
// what creating a migration for it *right now* would actually get from
// this policy -- the same two decisions kairon-controller's own migration
// reconcile loop makes for every migration, computed by calling its own
// exported, pure controller.AdmitMigrationPolicy/
// controller.BandwidthMbpsFromPolicies rather than re-deriving either
// decision, so this preview can never drift from what the controller will
// actually do (identical justification, and identical mechanism, to
// describeSnapshotSchedule calling the schedule's own Spec.Due/
// DeadlineExceeded).
//
// Both decisions depend on state beyond this one policy, which this preview
// surfaces rather than hides: AdmitMigrationPolicy spends a shared
// per-policy MaxConcurrent slot as it's asked about each matching Machine
// in turn -- mirroring a real sequential batch of migration creations, not
// a static "would it fit in isolation" check per Machine -- so Machines are
// walked in the same stable (name-sorted) order every run, and once a
// policy's own cap is exhausted, every remaining match correctly reports
// blocked, exactly like a real batch created in that order would.
// BandwidthMbpsFromPolicies additionally depends on every other
// MigrationPolicy in the namespace, not just this one: when an earlier
// (list-order) policy also matches a Machine and sets its own
// spec.bandwidthMbps, that other policy's value wins over this policy's --
// firstOverlappingBandwidthPolicy below identifies which policy actually
// won by name, so this never misattributes another policy's number as this
// policy's own (a real, silent surprise this preview exists to head off).
func describeMigrationPolicy(ctx context.Context, kc *kube.Client, ns, name string) {
	policy, err := kc.GetMigrationPolicy(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(policy, "", "  ")
	fmt.Println(string(b))

	machines, err := kc.ListMachinesNamespace(ctx, ns)
	if err != nil {
		fatal(err)
	}
	migrations, err := kc.ListMachineMigrationsNamespace(ctx, ns)
	if err != nil && !kube.IsNotFound(err) {
		fatal(err)
	}
	policies, err := kc.ListMigrationPoliciesNamespace(ctx, ns)
	if err != nil && !kube.IsNotFound(err) {
		fatal(err)
	}
	states := controller.LoadMigrationPolicyStates(policies, machines, migrations)

	var matches []model.Machine
	for _, m := range machines {
		if model.LabelsMatch(m.Metadata.Labels, policy.Spec.Selector) {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Metadata.Name < matches[j].Metadata.Name })

	fmt.Println()
	limit := "unlimited"
	if policy.Spec.MaxConcurrent > 0 {
		limit = fmt.Sprintf("%d", policy.Spec.MaxConcurrent)
	}
	fmt.Printf("Matching machines (%d) -- status.activeMigrations %d/%s; a migration created for each of these right now, in this order, would get:\n", len(matches), policy.Status.ActiveMigrations, limit)
	if len(matches) == 0 {
		fmt.Println("  (none -- check spec.selector against these Machines' own labels)")
		return
	}
	for _, m := range matches {
		verdict := "admitted"
		if blocker := controller.AdmitMigrationPolicy(states, m); blocker != "" {
			verdict = "BLOCKED (" + blocker + ")"
		}
		bwLine := "no bandwidth override"
		if bw := controller.BandwidthMbpsFromPolicies(states, m); bw > 0 {
			if winner := firstOverlappingBandwidthPolicy(policies, m); winner == policy.Metadata.Name {
				bwLine = fmt.Sprintf("%d Mbps (this policy)", bw)
			} else {
				bwLine = fmt.Sprintf("%d Mbps (from %q, an earlier-matching MigrationPolicy, not this one)", bw, winner)
			}
		}
		if a := controller.ResolveMigrationTuning(states, m).Adaptive; a != nil {
			if a.Policy == policy.Metadata.Name {
				bwLine += ", adaptive (this policy)"
			} else {
				bwLine += fmt.Sprintf(", adaptive (from %q)", a.Policy)
			}
		}
		fmt.Printf("  %-24s %-40s %s\n", m.Metadata.Name, verdict, bwLine)
	}
}

// describeMigration prints the raw MachineMigration, then a readable
// progress block for live transfers.
func describeMigration(ctx context.Context, kc *kube.Client, ns, name string) {
	mig, err := kc.GetMachineMigration(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(mig, "", "  ")
	fmt.Println(string(b))
	if block := migrationProgressBlock(mig.Status); block != "" {
		fmt.Println()
		fmt.Print(block)
	}
}

func migrationProgressBlock(st model.MachineMigrationStatus) string {
	if st.RAMTotal == 0 && st.Progress == nil && st.Adaptive == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Progress (%s):\n", st.Phase)
	if st.RAMTotal > 0 {
		pct := st.RAMTransferred * 100 / st.RAMTotal
		fmt.Fprintf(&b, "  Transferred:        %s of %s (%d%%), %s remaining\n", humanBytes(st.RAMTransferred), humanBytes(st.RAMTotal), min(pct, 100), humanBytes(st.RAMRemaining))
	}
	if p := st.Progress; p != nil {
		fmt.Fprintf(&b, "  Throughput:         %d Mbps\n", p.ThroughputMbps)
		fmt.Fprintf(&b, "  Dirty rate:         %d Mbps (%d pages/s)\n", p.DirtyRateMbps, p.DirtyPagesRate)
		fmt.Fprintf(&b, "  Expected downtime:  %d ms\n", p.ExpectedDowntimeMs)
		if p.CPUThrottlePct > 0 {
			fmt.Fprintf(&b, "  vCPU throttle:      %d%%\n", p.CPUThrottlePct)
		}
		if p.CompressionRatioPercent > 0 {
			fmt.Fprintf(&b, "  Compression ratio:  %d.%02dx\n", p.CompressionRatioPercent/100, p.CompressionRatioPercent%100)
		}
		if p.DuplicatePages > 0 {
			fmt.Fprintf(&b, "  Duplicate pages:    %d\n", p.DuplicatePages)
		}
		if p.EstimatedCompletionSeconds > 0 {
			fmt.Fprintf(&b, "  Estimated finish:   %s\n", (time.Duration(p.EstimatedCompletionSeconds) * time.Second).String())
		} else {
			fmt.Fprintf(&b, "  Estimated finish:   unknown (the guest dirties memory as fast as the link drains it)\n")
		}
	}
	if st.DowntimeMs > 0 {
		fmt.Fprintf(&b, "  Downtime:           %d ms\n", st.DowntimeMs)
	}
	if a := st.Adaptive; a != nil {
		fmt.Fprintf(&b, "Adaptive (experimental):\n")
		fmt.Fprintf(&b, "  Stage:              %s\n", a.Stage)
		if a.Message != "" {
			fmt.Fprintf(&b, "  Message:            %s\n", a.Message)
		}
		if a.BandwidthMbps > 0 || a.DowntimeLimitMs > 0 {
			fmt.Fprintf(&b, "  Applied:            bandwidth %d Mbps, downtime limit %d ms, vCPU throttle ceiling %d%%\n", a.BandwidthMbps, a.DowntimeLimitMs, a.MaxThrottlePct)
		}
		switch {
		case a.PostCopyStarted:
			fmt.Fprintf(&b, "  Post-copy:          started -- a failure now needs operator recovery\n")
		case a.PostCopyArmed:
			fmt.Fprintf(&b, "  Post-copy:          armed\n")
		}
		for _, act := range a.Actions {
			fmt.Fprintf(&b, "  %s  %-16s %s\n", act.At.UTC().Format(time.RFC3339), act.Action, act.Detail)
		}
	}
	return b.String()
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// firstOverlappingBandwidthPolicy names the first (list-order) policy that
// both matches m's labels and sets a nonzero spec.bandwidthMbps -- the same
// selection controller.BandwidthMbpsFromPolicies makes internally to pick a
// *value*, walked here only to attach a human-readable name to that value
// for describeMigrationPolicy's output, never to make or duplicate the
// admission/bandwidth decision itself.
func firstOverlappingBandwidthPolicy(policies []model.MigrationPolicy, m model.Machine) string {
	for _, p := range policies {
		if model.LabelsMatch(m.Metadata.Labels, p.Spec.Selector) && p.Spec.MaxBandwidthMbps() > 0 {
			return p.Metadata.Name
		}
	}
	return ""
}

// describeQuota is describe's third raw-JSON-plus-preview exception (see
// describeSnapshotSchedule's doc comment above for why these are
// deliberately narrow, not a generalized richer-describe change). A
// MachineQuota's raw JSON already contains both spec.maxTotalCpu/
// maxTotalMemory (human strings like "8Gi") and status.usedTotalCpuCores/
// usedTotalMemoryMiB (normalized numbers, written by kairon-controller's
// own reconcileQuotasStatus-equivalent tally every tick) -- but they sit in
// two disjoint top-level objects, in two different unit systems, so reading
// "how close is this namespace to its cap" out of the raw dump means
// mentally converting a Gi string to MiB and cross-referencing it against a
// separate field by eye. This preview does that conversion once, lines used
// up against limit in matching units the way `kubectl describe
// resourcequota` lines up its own Used/Hard columns, and additionally lists
// exactly which Machines are counted -- the same "don't just report a
// number, show the receipts" precedent describeSnapshotSchedule's own
// "Matching machines" list and describeMigrationPolicy's per-Machine
// breakdown already established.
//
// Usage is recomputed fresh from the namespace's current Machines via
// controller.MachineCountsTowardQuota/MachineFootprint -- the exact
// predicate and footprint calculation kairon-controller's own scheduling
// loop uses to admit or block a new Machine (see internal/controller/
// quota.go) -- rather than trusted from status, which is only ever as
// fresh as the last reconcile tick's patch. That also means this preview's
// totals can very occasionally read a few Machines ahead of status.used* if
// run between a Machine's admission and the next tick's status patch; that
// is a real, honest gap (not a bug to paper over), so the header below says
// "recomputed fresh" rather than implying it's reading status verbatim.
func describeQuota(ctx context.Context, kc *kube.Client, ns, name string) {
	quota, err := kc.GetMachineQuota(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(quota, "", "  ")
	fmt.Println(string(b))

	machines, err := kc.ListMachinesNamespace(ctx, ns)
	if err != nil {
		fatal(err)
	}
	var counted []model.Machine
	for _, m := range machines {
		if controller.MachineCountsTowardQuota(m) {
			counted = append(counted, m)
		}
	}
	sort.Slice(counted, func(i, j int) bool { return counted[i].Metadata.Name < counted[j].Metadata.Name })

	var usedMachines int
	var usedCPU uint32
	var usedMemMiB uint64
	for _, m := range counted {
		usedMachines++
		cpu, mem := controller.MachineFootprint(m)
		usedCPU += cpu
		usedMemMiB += mem
	}

	fmt.Println()
	fmt.Println("Usage (recomputed fresh from Machines counted right now -- same MachineCountsTowardQuota predicate kairon-controller's own scheduling loop uses):")
	if quota.Spec.MaxMachines != nil {
		fmt.Printf("  %-9s %d / %d\n", "machines", usedMachines, *quota.Spec.MaxMachines)
	} else {
		fmt.Printf("  %-9s %d / (no limit)\n", "machines", usedMachines)
	}
	if quota.Spec.MaxTotalCPU != "" {
		max, _ := model.ParseVCPUs(quota.Spec.MaxTotalCPU) // already admitted onto this object, so already valid
		fmt.Printf("  %-9s %d vCPU / %d vCPU (spec.maxTotalCpu %q; %d vCPU headroom)\n", "cpu", usedCPU, max, quota.Spec.MaxTotalCPU, int64(max)-int64(usedCPU))
	} else {
		fmt.Printf("  %-9s %d vCPU / (no limit)\n", "cpu", usedCPU)
	}
	if quota.Spec.MaxTotalMemory != "" {
		max, _ := model.ParseMemoryMiB(quota.Spec.MaxTotalMemory) // already admitted onto this object, so already valid
		fmt.Printf("  %-9s %d MiB / %d MiB (spec.maxTotalMemory %q; %d MiB headroom)\n", "memory", usedMemMiB, max, quota.Spec.MaxTotalMemory, int64(max)-int64(usedMemMiB))
	} else {
		fmt.Printf("  %-9s %d MiB / (no limit)\n", "memory", usedMemMiB)
	}

	fmt.Println()
	fmt.Printf("Counted machines (%d):\n", len(counted))
	if len(counted) == 0 {
		fmt.Println("  (none -- no scheduled, non-Stopped/Halted Machine in this namespace counts against this quota right now)")
		return
	}
	for _, m := range counted {
		cpu, mem := controller.MachineFootprint(m)
		fmt.Printf("  %-24s %d vCPU  %d MiB\n", m.Metadata.Name, cpu, mem)
	}
}

// describeBudget is describe's fourth (and, for now, last -- see
// describeQuota's own doc comment for the third) raw-JSON-plus-preview
// exception. Unlike MachineQuota's status, a MachineDisruptionBudget's
// status.expectedMachines/currentHealthy/desiredHealthy/disruptionsAllowed
// are already four clearly-labeled, already-resolved counts in the same
// unit (Machines) -- reading the raw JSON tells an operator the *numbers*
// perfectly well on its own. What it can't tell them is *which* Machines
// are counted, and specifically *why* any of them isn't currently healthy
// -- the same "show the receipts behind the number" gap describeQuota's
// counted-machines list and describeSnapshotSchedule's/
// describeMigrationPolicy's own matching-machines lists already exist to
// close for their own CRDs. That's the one genuine addition this preview
// makes: list every Machine spec.selector currently matches, and for any
// that isn't counted as healthy, say whether it's because status.phase
// isn't Running or because a non-terminal MachineMigration already has it
// in flight.
//
// Status is recomputed fresh via controller.LoadBudgetStates -- the exact
// function both `kaironctl evacuate` and the opt-in admission webhook call
// to decide the same thing -- rather than trusted from status.*, which
// (like MachineQuota's) is only ever as fresh as the last reconcile tick.
// The per-Machine in-flight check below deliberately duplicates
// LoadBudgetStates' own inline terminal-phase test rather than importing
// it (that helper is unexported, and internal/controller/disruption.go's
// BudgetState doc comment already establishes this codebase's precedent of
// each consumer keeping its own narrow copy of a one-boolean predicate
// rather than exporting it for a single caller) -- so if that set of
// terminal phases ever changes, this comment is the reminder to update
// both places together.
func describeBudget(ctx context.Context, kc *kube.Client, ns, name string) {
	budget, err := kc.GetMachineDisruptionBudget(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(budget, "", "  ")
	fmt.Println(string(b))

	machines, err := kc.ListMachinesNamespace(ctx, ns)
	if err != nil {
		fatal(err)
	}
	migrations, err := kc.ListMachineMigrationsNamespace(ctx, ns)
	if err != nil && !kube.IsNotFound(err) {
		fatal(err)
	}
	states, err := controller.LoadBudgetStates([]model.MachineDisruptionBudget{budget}, machines, migrations)
	if err != nil {
		fatal(err)
	}
	status := states[0].Status()

	inFlight := map[string]bool{}
	for _, mig := range migrations {
		terminal := false
		switch mig.Status.Phase {
		case "Succeeded", "Failed", "Blocked", "Cancelled", "":
			terminal = true
		}
		if !terminal {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}

	var matches []model.Machine
	for _, m := range machines {
		if model.LabelsMatch(m.Metadata.Labels, budget.Spec.Selector) {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Metadata.Name < matches[j].Metadata.Name })

	fmt.Println()
	fmt.Printf("Status (recomputed fresh from Machines/MachineMigrations right now): %d matching, %d healthy, %d desired healthy, %d disruptions allowed\n",
		status.ExpectedMachines, status.CurrentHealthy, status.DesiredHealthy, status.DisruptionsAllowed)
	fmt.Printf("Matching machines (%d):\n", len(matches))
	if len(matches) == 0 {
		fmt.Println("  (none -- check spec.selector against these Machines' own labels)")
		return
	}
	for _, m := range matches {
		health := "healthy"
		switch {
		case m.Status.Phase != "Running":
			health = fmt.Sprintf("NOT healthy (status.phase %q, not Running)", m.Status.Phase)
		case inFlight[m.Namespace()+"/"+m.Metadata.Name]:
			health = "NOT healthy (non-terminal MachineMigration in flight)"
		}
		fmt.Printf("  %-24s %s\n", m.Metadata.Name, health)
	}
}

// describeMachine is describe's fifth (and, for now, last -- see
// describeBudget's own doc comment for the fourth) raw-JSON-plus-preview
// exception, and the narrowest of the five: it prints the exact same raw
// JSON dump the default case already produces for every other kind, then
// appends exactly one extra line, and only when spec.tenant is actually
// set. Machine.Spec.Tenant is read by nothing anywhere in this codebase --
// no admission check, no controller logic, no kaironctl flag, no dashboard
// display (see its own doc comment in internal/model/types.go) -- so an
// operator who sets it could easily assume it does something. This note
// exists purely to make that non-enforcement impossible to miss at the one
// place an operator is most likely to go looking: describing the Machine
// itself.
func describeMachine(ctx context.Context, kc *kube.Client, ns, name string) {
	m, err := kc.GetMachine(ctx, ns, name)
	if err != nil {
		fatal(err)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))

	if line := elasticMemorySummary(m); line != "" {
		fmt.Println(line)
	}
	if m.Spec.Tenant != "" {
		fence := "off"
		if m.Metadata.Annotations["kairon.zyvor.dev/tenant-fence"] == "true" {
			fence = "on"
		}
		fmt.Printf("note: spec.tenant is an east-west network fence only with kairon.zyvor.dev/tenant-fence=true (%s here) -- Kubernetes Namespace (%q) is still the quota and RBAC boundary; see docs/guides/tenant-fence.md\n", fence, m.Namespace())
	}
}

// cmdTrigger dispatches `kaironctl trigger KIND NAME` -- deliberately its
// own top-level verb, not folded into `edit`, because it isn't editing any
// field a person would want to read back afterward (unlike every `edit`
// flag): it's a one-shot imperative request, the same shape
// cancel-migration already established for "ask the controller to do a
// thing now" rather than "change this object's configuration." Only
// snapshotschedule supports it today -- MigrationPolicy/
// MachineDisruptionBudget/etc. have nothing analogous to "run this right

// elasticMemorySummary is the one-line reading of status.memory: how much
// of the guest's memory kairon-node has ballooned away and why.
func elasticMemorySummary(m model.Machine) string {
	em := m.Spec.Resources.ElasticMemory
	if em == nil {
		return ""
	}
	st := m.Status.Memory
	if st == nil {
		return fmt.Sprintf("elastic memory: min %s, no reclaim status yet (kairon-node writes it once the Machine runs)", em.Min)
	}
	inGuest := st.CurrentMiB
	if st.BalloonActualMiB < inGuest {
		inGuest -= st.BalloonActualMiB
	}
	line := fmt.Sprintf("elastic memory: %dMi of %dMi in guest (min %dMi), balloon target %dMi, actual %dMi",
		inGuest, st.CurrentMiB, st.MinMiB, st.BalloonTargetMiB, st.BalloonActualMiB)
	if st.GuestAvailableMiB != nil {
		line += fmt.Sprintf(", guest available %dMi", *st.GuestAvailableMiB)
	}
	if st.Reason != "" {
		line += fmt.Sprintf(" (%s at %s)", st.Reason, st.LastChangeTime)
	}
	return line
}
