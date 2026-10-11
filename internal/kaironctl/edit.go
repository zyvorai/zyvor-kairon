// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
)

func cmdEdit(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 2 {
		fatal(fmt.Errorf("usage: kaironctl edit migrationpolicy NAME [--bandwidth-mbps N] [--max-concurrent N] [--strategy static|adaptive] [tuning flags] | edit snapshotschedule NAME [--suspend true|false] [--interval-seconds N] [--keep-last N] [--starting-deadline-seconds N] | edit quota NAME [--max-machines N] [--max-total-cpu N] [--max-total-memory SIZE] | edit budget NAME [--selector k=v] [--min-available X] [--max-unavailable X] | edit machine NAME --priority N | edit machineset NAME [--strategy RollingUpdate|Recreate] [--max-unavailable X] | edit networkpolicy NAME [--machine-name X] [--selector k=v] [--allow-cidr CIDR] [--deny-cidr CIDR] [--allow-port proto/port] [--default-allow BOOL] [--audit-mode BOOL] [--max-egress-mbps N] [--max-egress-pps N] | edit securitygroup NAME [--group-label k=v] [--priority N] [--description TEXT] [policy flags as above]"))
	}
	kind, name := strings.ToLower(args[0]), args[1]
	switch kind {
	case "migrationpolicy", "migrationpolicies":
		cmdEditMigrationPolicy(ctx, kc, name, args[2:])
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		cmdEditSnapshotSchedule(ctx, kc, name, args[2:])
	case "quota", "quotas", "machinequotas":
		cmdEditQuota(ctx, kc, name, args[2:])
	case "budget", "budgets", "machinedisruptionbudgets":
		cmdEditBudget(ctx, kc, name, args[2:])
	case "machine", "machines":
		cmdEditMachine(ctx, kc, name, args[2:])
	case "machineset", "machinesets":
		cmdEditMachineSet(ctx, kc, name, args[2:])
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		cmdEditNetworkPolicy(ctx, kc, name, args[2:])
	case "securitygroup", "securitygroups", "networksecuritygroups":
		cmdEditSecurityGroup(ctx, kc, name, args[2:])
	default:
		fatal(fmt.Errorf("edit only supports migrationpolicy, snapshotschedule, quota, budget, machine, machineset, networkpolicy, or securitygroup, got %q", kind))
	}
}

// cmdEditMachine patches only spec.priority for a first cut -- the one
// Machine-spec field this project considers safe to change on an
// already-created (possibly already-scheduled) Machine via a narrow
// merge-patch, since it's purely an admission-order hint for a future
// reconcile tick (see MachineSpec.Priority's own doc comment) and never
// itself triggers a re-realization. Every other Machine-spec field
// (image, resources, network, ...) stays create-time-only through this
// CLI, same as before `edit machine` existed -- kubectl apply/YAML is
// still the only way to change those, exactly like the fields `create`
// itself doesn't expose flags for.
func cmdEditMachine(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit machine", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	priority := fs.Int("priority", 0, "new scheduling priority (higher wins a scarce reconcile-tick race for node capacity or MachineQuota headroom; see kaironctl create --priority)")
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "priority" {
			spec["priority"] = *priority
		}
	})
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass --priority"))
	}
	if err := kc.PatchMachine(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("machine/%s updated", name)
}

// cmdEditMachineSet patches only the MachineSetSpec rollout knobs an
// explicit flag was passed for, same fs.Visit convention as every other
// edit subcommand -- Replicas deliberately stays out of this verb's scope
// since `kaironctl scale machineset` already exists specifically for it
// (see cmdScale's own doc comment for why scale and edit are kept
// separate), and Template is create-time-only through this CLI exactly
// like a Machine's own image/resources/network fields are under
// cmdEditMachine -- kubectl edit/apply is still how a replica's template
// itself changes. --strategy/--max-unavailable are otherwise the entire
// set of fields cmdCreateMachineSet accepts beyond Replicas/Template, so
// this closes the one real gap left: before this, changing a MachineSet's
// rollout strategy or maxUnavailable bound after creation needed
// kubectl edit/patch, unlike every other CRD kind this project offers a
// `kaironctl edit` verb for at all.
// editSpecFromFlags implements the common "parse only the flags the
// caller actually passed, build a partial spec patch from them, apply
// it, print a confirmation" skeleton that cmdEditMachineSet and
// cmdEditMigrationPolicy both follow: fs must already have its flags
// defined, setField is called once per flag fs.Visit reports as
// explicitly set (to populate spec from whatever *flag.Value the
// caller's closure captured), emptyErr is the "nothing to edit: ..."
// detail used when no flag was passed, and patch applies the resulting
// spec (the caller wraps it as {"spec": spec}) via kube.Client.
func editSpecFromFlags(fs *flag.FlagSet, args []string, setField func(spec map[string]any, flagName string), emptyErr string, patch func(spec map[string]any) error, kind, resourceName string) {
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) { setField(spec, f.Name) })
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: %s", emptyErr))
	}
	if err := patch(spec); err != nil {
		fatal(err)
	}
	okf("%s/%s updated", kind, resourceName)
}

func cmdEditMachineSet(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit machineset", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	strategy := fs.String("strategy", "", "new rollout strategy: RollingUpdate | Recreate")
	maxUnavailable := fs.String("max-unavailable", "", "new integer or percentage bound on simultaneously-missing/outdated replicas during RollingUpdate")
	editSpecFromFlags(fs, args, func(spec map[string]any, flagName string) {
		switch flagName {
		case "strategy":
			spec["strategy"] = *strategy
		case "max-unavailable":
			spec["maxUnavailable"] = *maxUnavailable
		}
	}, "pass at least one of --strategy or --max-unavailable", func(spec map[string]any) error {
		return kc.PatchMachineSet(ctx, *ns, name, map[string]any{"spec": spec})
	}, "machineset", name)
}

func cmdEditMigrationPolicy(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit migrationpolicy", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	bandwidth := fs.Uint64("bandwidth-mbps", 0, "new default migration bandwidth")
	maxConcurrent := fs.Int("max-concurrent", 0, "new cap on simultaneous non-terminal migrations")
	var tuning policyTuningFlags
	tuning.register(fs)
	editSpecFromFlags(fs, args, func(spec map[string]any, flagName string) {
		switch flagName {
		case "bandwidth-mbps":
			spec["bandwidthMbps"] = *bandwidth
		case "max-concurrent":
			spec["maxConcurrent"] = *maxConcurrent
		default:
			tuningPatch, err := tuning.patch(visited(fs))
			if err != nil {
				fatal(err)
			}
			maps.Copy(spec, tuningPatch)
		}
	}, "pass at least one of --bandwidth-mbps, --max-concurrent or a tuning flag such as --strategy", func(spec map[string]any) error {
		return kc.PatchMigrationPolicy(ctx, *ns, name, map[string]any{"spec": spec})
	}, "migrationpolicy", name)
}

func cmdEditSnapshotSchedule(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit snapshotschedule", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	suspend := fs.Bool("suspend", false, "pause (true) or resume (false) this schedule")
	intervalSeconds := fs.Int("interval-seconds", 0, "new minimum seconds between runs (minimum 60)")
	keepLast := fs.Int("keep-last", 0, "retain only the N most recent ready-to-use snapshots this schedule created per Machine (0 disables pruning again)")
	startingDeadlineSeconds := fs.Int("starting-deadline-seconds", 0, "skip (rather than immediately fire) a run found more than this many seconds late (0 disables the deadline again -- an overdue run always fires)")
	dailyAt := fs.String("daily-at", "", "switch to once a day at HH:MM UTC (clears intervalSeconds)")
	jitterSeconds := fs.Int("jitter-seconds", 0, "stable per-schedule offset in [0, N) seconds (0 disables)")
	maxAgeSeconds := fs.Int("max-age-seconds", 0, "prune this schedule's snapshots older than N seconds (0 disables)")
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "daily-at":
			spec["dailyAt"] = *dailyAt
			spec["intervalSeconds"] = nil
		case "jitter-seconds":
			spec["jitterSeconds"] = *jitterSeconds
		case "max-age-seconds":
			spec["maxAgeSeconds"] = *maxAgeSeconds
		case "suspend":
			spec["suspend"] = *suspend
		case "interval-seconds":
			spec["intervalSeconds"] = *intervalSeconds
			if _, ok := spec["dailyAt"]; !ok {
				spec["dailyAt"] = nil
			}
		case "keep-last":
			spec["keepLast"] = *keepLast
		case "starting-deadline-seconds":
			spec["startingDeadlineSeconds"] = *startingDeadlineSeconds
		}
	})
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass at least one of --suspend, --interval-seconds, --daily-at, --jitter-seconds, --keep-last, --max-age-seconds, or --starting-deadline-seconds"))
	}
	if err := kc.PatchMachineSnapshotSchedule(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("snapshotschedule/%s updated", name)
}

// cmdEditQuota patches only the MachineQuota dimensions an explicit flag
// was passed for, same fs.Visit convention as cmdEditMigrationPolicy/
// cmdEditSnapshotSchedule above -- an omitted flag never clobbers an
// already-configured cap back to "unset."
func cmdEditQuota(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit quota", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	maxMachines := fs.Int("max-machines", 0, "new cap on scheduled Machine count in this namespace")
	maxTotalCPU := fs.String("max-total-cpu", "", "new cap on total vCPUs scheduled in this namespace")
	maxTotalMemory := fs.String("max-total-memory", "", "new cap on total memory scheduled in this namespace")
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "max-machines":
			spec["maxMachines"] = *maxMachines
		case "max-total-cpu":
			spec["maxTotalCpu"] = *maxTotalCPU
		case "max-total-memory":
			spec["maxTotalMemory"] = *maxTotalMemory
		}
	})
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass at least one of --max-machines, --max-total-cpu, or --max-total-memory"))
	}
	if err := kc.PatchMachineQuota(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("quota/%s updated", name)
}

// cmdEditBudget patches only the MachineDisruptionBudget fields an explicit
// flag was passed for, same fs.Visit convention as the other edit
// subcommands. Passing both --min-available and --max-unavailable in the
// same call is refused, matching cmdCreateBudget's own exactly-one
// validation -- passing only one of them is always fine, including to
// switch a budget from one bound type to the other (the apiserver-side
// merge patch leaves the other field's prior value in place, so a full
// switch needs a follow-up kubectl/YAML edit to clear the old field, a
// known first-cut limit of this verb's simple merge-patch shape).
func cmdEditBudget(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit budget", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "new label key=value selector (repeatable; replaces the entire existing selector when passed)")
	minAvailable := fs.String("min-available", "", "new integer or \"N%\" floor on healthy matching Machines")
	maxUnavailable := fs.String("max-unavailable", "", "new integer or \"N%\" ceiling on unhealthy/disrupted matching Machines")
	_ = fs.Parse(args)
	if *minAvailable != "" && *maxUnavailable != "" {
		fatal(fmt.Errorf("cannot pass both --min-available and --max-unavailable in the same edit"))
	}
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "selector":
			selectorMap, err := parseKeyValues(selector)
			if err != nil {
				fatal(err)
			}
			spec["selector"] = selectorMap
		case "min-available":
			spec["minAvailable"] = *minAvailable
		case "max-unavailable":
			spec["maxUnavailable"] = *maxUnavailable
		}
	})
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass at least one of --selector, --min-available, or --max-unavailable"))
	}
	if err := kc.PatchMachineDisruptionBudget(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("budget/%s updated", name)
}

// editableVmNetworkPolicyFlags registers the subset of model.VmNetworkPolicy
// fields this project considers safe to change in place on an existing
// MachineNetworkPolicy/NetworkSecurityGroup -- the same "first cut, narrower
// than create" scoping cmdEditMachine already applies to Machine.spec (see
// its own doc comment): --allow-cidr/--deny-cidr/--allow-port/
// --default-allow/--audit-mode/--max-egress-mbps/--max-egress-pps cover the
// routine "widen/narrow this rule" and "flip to dry-run before enforcing"
// edits an incident or a policy review actually needs day to day.
// --allow-fqdn/--policy-group/--policy-label/--entity/--allow-icmp/
// --sample-rate stay create-time-only through this CLI for now; kubectl
// edit/apply is still how those change, exactly like every Machine-spec
// field cmdEditMachine doesn't expose a flag for. Returns the JSON-tag-keyed
// patch fragment for spec.policy, or nil if nothing was touched -- the
// caller merges it under "policy" only when non-nil, so an edit that
// changes none of these fields (e.g. only --selector) never sends a bogus
// empty policy object.
func editableVmNetworkPolicyFlags(fs *flag.FlagSet) func() map[string]any {
	var allowCidrs, denyCidrs, allowPorts stringSliceFlag
	fs.Var(&allowCidrs, "allow-cidr", "new destination CIDRs to allow (repeatable; replaces the entire existing list when passed)")
	fs.Var(&denyCidrs, "deny-cidr", "new destination CIDRs to deny (repeatable; replaces the entire existing list when passed)")
	fs.Var(&allowPorts, "allow-port", "new proto/port rules to allow, e.g. tcp/443 (repeatable; replaces the entire existing list when passed)")
	defaultAllow := fs.Bool("default-allow", false, "new default-allow setting")
	auditMode := fs.Bool("audit-mode", false, "new audit-mode setting (true logs would-be-denied traffic instead of dropping it)")
	maxEgressMbps := fs.Uint64("max-egress-mbps", 0, "new egress bandwidth cap in Mbps")
	maxEgressPps := fs.Uint64("max-egress-pps", 0, "new egress packet-rate cap in packets/sec")
	return func() map[string]any {
		policy := map[string]any{}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "allow-cidr":
				policy["allowCidrs"] = []string(allowCidrs)
			case "deny-cidr":
				policy["denyCidrs"] = []string(denyCidrs)
			case "allow-port":
				policy["allowPorts"] = []string(allowPorts)
			case "default-allow":
				policy["defaultAllow"] = *defaultAllow
			case "audit-mode":
				policy["auditMode"] = *auditMode
			case "max-egress-mbps":
				policy["maxEgressMbps"] = *maxEgressMbps
			case "max-egress-pps":
				policy["maxEgressPps"] = *maxEgressPps
			}
		})
		if len(policy) == 0 {
			return nil
		}
		return policy
	}
}

// cmdEditNetworkPolicy patches only the MachineNetworkPolicy fields an
// explicit flag was passed for, same fs.Visit convention as every other
// edit subcommand -- see editableVmNetworkPolicyFlags's own doc comment for
// exactly which spec.policy fields this covers.
func cmdEditNetworkPolicy(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit networkpolicy", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	machineName := fs.String("machine-name", "", "new single-Machine target; takes precedence over spec.selector")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "new label key=value selector (repeatable; replaces the entire existing selector when passed)")
	policyFn := editableVmNetworkPolicyFlags(fs)
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "machine-name":
			spec["machineName"] = *machineName
		case "selector":
			selectorMap, err := parseKeyValues(selector)
			if err != nil {
				fatal(err)
			}
			spec["selector"] = selectorMap
		}
	})
	if policy := policyFn(); policy != nil {
		spec["policy"] = policy
	}
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass at least one of --machine-name, --selector, --allow-cidr, --deny-cidr, --allow-port, --default-allow, --audit-mode, --max-egress-mbps, or --max-egress-pps"))
	}
	if err := kc.PatchMachineNetworkPolicy(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("networkpolicy/%s updated", name)
}

// cmdEditSecurityGroup is cmdEditNetworkPolicy's exact counterpart for
// NetworkSecurityGroup, plus its three own top-level spec fields
// (--group-label/--priority/--description) in place of
// --machine-name/--selector.
func cmdEditSecurityGroup(ctx context.Context, kc *kube.Client, name string, args []string) {
	fs := flag.NewFlagSet("edit securitygroup", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var groupLabels stringSliceFlag
	fs.Var(&groupLabels, "group-label", "new key=value tags FluxVM matches group membership against (repeatable; replaces the entire existing list when passed)")
	priority := fs.Uint("priority", 0, "new priority (lower wins on a rate/deny tie against another group)")
	description := fs.String("description", "", "new human-readable note")
	policyFn := editableVmNetworkPolicyFlags(fs)
	_ = fs.Parse(args)
	spec := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "group-label":
			spec["labels"] = []string(groupLabels)
		case "priority":
			spec["priority"] = *priority
		case "description":
			spec["description"] = *description
		}
	})
	if policy := policyFn(); policy != nil {
		spec["policy"] = policy
	}
	if len(spec) == 0 {
		fatal(fmt.Errorf("nothing to edit: pass at least one of --group-label, --priority, --description, --allow-cidr, --deny-cidr, --allow-port, --default-allow, --audit-mode, --max-egress-mbps, or --max-egress-pps"))
	}
	if err := kc.PatchNetworkSecurityGroup(ctx, *ns, name, map[string]any{"spec": spec}); err != nil {
		fatal(err)
	}
	okf("securitygroup/%s updated", name)
}

// cmdDelete deletes either a single named resource (`delete [RESOURCE]
// NAME`, the original and still-default calling convention -- unchanged
// below) or, once --selector appears anywhere in args, every resource of
// an explicitly named RESOURCE kind whose labels satisfy every given
// key=value pair (`delete RESOURCE --selector k=v [--selector k2=v2]
// [--dry-run]`). Every other kaironctl verb addresses exactly one named
// object; cleaning up, say, every Machine a finished load test left
// behind meant either a shell loop around `kaironctl get -o` output or
// reaching for kubectl. hasFlag, not resourceKindAndName's own strict
// "1 or 2 positional args" arithmetic, is what tells the two calling
