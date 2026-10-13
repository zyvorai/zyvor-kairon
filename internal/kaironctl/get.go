// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdGet(ctx context.Context, kc *kube.Client, args []string) {
	format, args, err := extractOutput(args)
	if err != nil {
		fatal(err)
	}
	ns, args := nsFlag(args)
	resource := "machines"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		resource = strings.ToLower(args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	var selectorFlag stringSliceFlag
	fs.Var(&selectorFlag, "selector", "label key=value every listed resource must carry (repeatable -- every pair must match); omitted lists every resource of this kind, exactly as before this flag existed")
	_ = fs.Parse(args)
	selector, err := parseKeyValues(selectorFlag)
	if err != nil {
		fatal(err)
	}
	if format != "" {
		if err := getStructured(ctx, kc, os.Stdout, resource, ns, selector, format); err != nil {
			fatal(err)
		}
		return
	}
	switch resource {
	case "machine", "machines", "vm", "vms":
		items, err := kc.ListMachinesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(m model.Machine) map[string]string { return m.Metadata.Labels })
		fmt.Printf("NAME\tNODE\tPHASE\tCPU\tMEMORY\tIP\n")
		for _, m := range items {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", m.Metadata.Name, dash(m.Spec.NodeName), style.Phase(os.Stdout, m.Status.Phase), m.Spec.Resources.CPU, m.Spec.Resources.Memory, dash(m.Status.GuestIP))
		}
	case "migration", "migrations", "machinemigrations":
		items, err := kc.ListMachineMigrationsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(m model.MachineMigration) map[string]string { return m.Metadata.Labels })
		fmt.Printf("NAME\tMACHINE\tSTRATEGY\tSOURCE\tTARGET\tPHASE\n")
		for _, m := range items {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", m.Metadata.Name, m.Spec.MachineName, dash(m.Status.EffectiveStrategy), dash(m.Status.SourceNode), dash(m.Status.TargetNode), style.Phase(os.Stdout, m.Status.Phase))
		}
	case "snapshot", "snapshots", "machinesnapshots":
		items, err := kc.ListMachineSnapshotsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(s model.MachineSnapshot) map[string]string { return s.Metadata.Labels })
		fmt.Printf("NAME\tMACHINE\tPHASE\tREADY\n")
		for _, s := range items {
			fmt.Printf("%s\t%s\t%s\t%s\n", s.Metadata.Name, s.Spec.MachineName, style.Phase(os.Stdout, s.Status.Phase), style.BoolReady(os.Stdout, s.Status.ReadyToUse))
		}
	case "restore", "restores", "machinesnapshotrestores":
		items, err := kc.ListMachineSnapshotRestoresNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(r model.MachineSnapshotRestore) map[string]string { return r.Metadata.Labels })
		fmt.Printf("NAME\tSNAPSHOT\tCLAIM\tPHASE\n")
		for _, r := range items {
			fmt.Printf("%s\t%s\t%s\t%s\n", r.Metadata.Name, r.Spec.SnapshotName, dash(r.Status.RestoredClaimName), style.Phase(os.Stdout, r.Status.Phase))
		}
	case "backup", "backups", "machinebackups":
		items, err := kc.ListMachineBackupsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(b model.MachineBackup) map[string]string { return b.Metadata.Labels })
		printBackups(items)
	case "backuprestore", "backuprestores", "machinebackuprestores":
		items, err := kc.ListMachineBackupRestoresNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(r model.MachineBackupRestore) map[string]string { return r.Metadata.Labels })
		fmt.Printf("NAME\tBACKUP\tMACHINE\tPHASE\tMESSAGE\n")
		for _, r := range items {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", r.Metadata.Name, r.Spec.BackupName, dash(r.Status.MachineName), style.Phase(os.Stdout, r.Status.Phase), dash(r.Status.Message))
		}
	case "quota", "quotas", "machinequotas":
		items, err := kc.ListMachineQuotasNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(q model.MachineQuota) map[string]string { return q.Metadata.Labels })
		fmt.Printf("NAME\tMAXMACHINES\tMAXCPU\tMAXMEMORY\tUSEDMACHINES\tUSEDCPU\tUSEDMEMORYMIB\n")
		for _, q := range items {
			maxMachines := "-"
			if q.Spec.MaxMachines != nil {
				maxMachines = strconv.Itoa(*q.Spec.MaxMachines)
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%d\t%d\t%d\n", q.Metadata.Name, maxMachines, dash(q.Spec.MaxTotalCPU), dash(q.Spec.MaxTotalMemory), q.Status.UsedMachines, q.Status.UsedTotalCPUCores, q.Status.UsedTotalMemoryMiB)
		}
	case "budget", "budgets", "machinedisruptionbudgets":
		items, err := kc.ListMachineDisruptionBudgetsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(b model.MachineDisruptionBudget) map[string]string { return b.Metadata.Labels })
		fmt.Printf("NAME\tMINAVAILABLE\tMAXUNAVAILABLE\tEXPECTED\tHEALTHY\tDESIRED\tALLOWED\n")
		for _, b := range items {
			fmt.Printf("%s\t%s\t%s\t%d\t%d\t%d\t%d\n", b.Metadata.Name, dash(b.Spec.MinAvailable), dash(b.Spec.MaxUnavailable), b.Status.ExpectedMachines, b.Status.CurrentHealthy, b.Status.DesiredHealthy, b.Status.DisruptionsAllowed)
		}
	case "machineset", "machinesets":
		items, err := kc.ListMachineSetsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(s model.MachineSet) map[string]string { return s.Metadata.Labels })
		fmt.Printf("NAME\tSTRATEGY\tREPLICAS\tREADY\tUPDATED\n")
		for _, s := range items {
			fmt.Printf("%s\t%s\t%d\t%d\t%d\n", s.Metadata.Name, dash(defaultStrategy(s.Spec.Strategy)), s.Spec.Replicas, s.Status.ReadyReplicas, s.Status.UpdatedReplicas)
		}
	case "machinepool", "machinepools":
		items, err := kc.ListMachinePoolsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(p model.MachinePool) map[string]string { return p.Metadata.Labels })
		fmt.Printf("NAME\tWARM\tREADY\tCLAIMED\n")
		for _, p := range items {
			fmt.Printf("%s\t%d\t%d\t%d\n", p.Metadata.Name, p.Spec.Replicas, p.Status.ReadyReplicas, p.Status.Claimed)
		}
	case "machineclaim", "machineclaims", "claim", "claims":
		items, err := kc.ListMachineClaimsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(c model.MachineClaim) map[string]string { return c.Metadata.Labels })
		fmt.Printf("NAME\tPOOL\tPHASE\tMACHINE\tBIND-MS\n")
		for _, c := range items {
			fmt.Printf("%s\t%s\t%s\t%s\t%d\n", c.Metadata.Name, c.Spec.PoolName, style.Phase(os.Stdout, dash(c.Status.Phase)), dash(c.Status.MachineName), c.Status.BindMillis)
		}
	case "instancetype", "instancetypes", "machineinstancetypes":
		items, err := kc.ListMachineInstanceTypesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(it model.MachineInstanceType) map[string]string { return it.Metadata.Labels })
		fmt.Printf("NAME\tCPU\tMEMORY\n")
		for _, it := range items {
			fmt.Printf("%s\t%s\t%s\n", it.Metadata.Name, dash(it.Spec.Resources.CPU), dash(it.Spec.Resources.Memory))
		}
	case "migrationpolicy", "migrationpolicies":
		items, err := kc.ListMigrationPoliciesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(p model.MigrationPolicy) map[string]string { return p.Metadata.Labels })
		fmt.Printf("NAME\tBANDWIDTHMBPS\tACTIVEMIGRATIONS\n")
		for _, p := range items {
			bw := "-"
			if p.Spec.BandwidthMbps != 0 {
				bw = strconv.FormatUint(p.Spec.BandwidthMbps, 10)
			}
			fmt.Printf("%s\t%s\t%d\n", p.Metadata.Name, bw, p.Status.ActiveMigrations)
		}
	case "virtualnetwork", "virtualnetworks", "vnet", "vnets":
		items, err := kc.ListVirtualNetworksNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(v model.VirtualNetwork) map[string]string { return v.Metadata.Labels })
		printVirtualNetworks(os.Stdout, items)
	case "backuppolicy", "backuppolicies", "machinebackuppolicies":
		items, err := kc.ListMachineBackupPoliciesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(p model.MachineBackupPolicy) map[string]string { return p.Metadata.Labels })
		printBackupPolicies(os.Stdout, items)
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		items, err := kc.ListMachineSnapshotSchedulesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(s model.MachineSnapshotSchedule) map[string]string { return s.Metadata.Labels })
		fmt.Printf("NAME\tINTERVALSECONDS\tSTARTINGDEADLINE\tSUSPEND\tLASTRUN\tLASTCOUNT\tNEXTRUN\n")
		for _, s := range items {
			lastRun := "-"
			if !s.Status.LastRunTime.IsZero() {
				lastRun = s.Status.LastRunTime.Format(time.RFC3339)
			}
			deadline := "-"
			if s.Spec.StartingDeadlineSeconds > 0 {
				deadline = strconv.Itoa(s.Spec.StartingDeadlineSeconds)
			}
			interval := strconv.Itoa(s.Spec.IntervalSeconds)
			if s.Spec.DailyAt != "" {
				interval = "daily@" + s.Spec.DailyAt
			}
			fmt.Printf("%s\t%s\t%s\t%t\t%s\t%d\t%s\n", s.Metadata.Name, interval, deadline, s.Spec.Suspend, lastRun, s.Status.LastRunSnapshotCount, formatNextRun(s))
		}
	// networkpolicy/securitygroup: MachineNetworkPolicy and
	// NetworkSecurityGroup drive FluxVM's real eBPF/TC enforcement (see
	// docs/guides/network-policy.md) but, unlike every other kind here,
	// previously had no kaironctl support at all -- an operator debugging
	// "why is my traffic blocked" could reach for the four network-*
	// diagnostic endpoints but couldn't even list the policy objects
	// themselves without kubectl.
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		items, err := kc.ListMachineNetworkPoliciesNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(p model.MachineNetworkPolicy) map[string]string { return p.Metadata.Labels })
		fmt.Printf("NAME\tTARGET\tDEFAULTALLOW\tPHASE\tSYNCED\n")
		for _, p := range items {
			target := dash(p.Spec.MachineName)
			if target == "-" && len(p.Spec.Selector) > 0 {
				target = fmt.Sprintf("selector(%d)", len(p.Spec.Selector))
			}
			fmt.Printf("%s\t%s\t%t\t%s\t%s\n", p.Metadata.Name, target, p.Spec.Policy.DefaultAllow, style.Phase(os.Stdout, p.Status.Phase), style.BoolReady(os.Stdout, p.Status.EffectiveSynced))
		}
	case "securitygroup", "securitygroups", "networksecuritygroups":
		items, err := kc.ListNetworkSecurityGroupsNamespace(ctx, ns)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(g model.NetworkSecurityGroup) map[string]string { return g.Metadata.Labels })
		fmt.Printf("NAME\tGROUPNAME\tPRIORITY\tDEFAULTALLOW\tPHASE\tAPPLIEDON\n")
		for _, g := range items {
			groupName := g.Spec.GroupName
			if groupName == "" {
				groupName = g.Metadata.Name
			}
			fmt.Printf("%s\t%s\t%d\t%t\t%s\t%s\n", g.Metadata.Name, groupName, g.Spec.Priority, g.Spec.Policy.DefaultAllow, style.Phase(os.Stdout, g.Status.Phase), dash(g.Status.AppliedOn))
		}
	// node/nodes: the real Kubernetes core Node object internal/scheduler
	// itself already reads (spec.taints, spec.unschedulable) to decide
	// where a Machine can land -- see internal/scheduler.go's own
	// toleratesTaint/eligible -- but which previously had no kaironctl
	// support at all, the exact same "no CLI access short of raw kubectl"
	// gap networkpolicy/securitygroup above closed for those two kinds.
	// That gap became sharper the moment Node taints started actually
	// gating scheduling (see 2fd0257): an operator asking "why won't my
	// Machine schedule onto NODE" now has a real reason to look at a
	// node's taints, and had no kaironctl way to do so. Unlike every other
	// case above, NODE is cluster-scoped -- ns (from --namespace/-n) is
	// simply unused here, exactly as it already is for any flag a given
	// kind's underlying API doesn't have.
	case "node", "nodes":
		items, err := kc.ListNodes(ctx)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(n model.Node) map[string]string { return n.Metadata.Labels })
		fmt.Printf("NAME\tREADY\tUNSCHEDULABLE\tTAINTS\n")
		for _, n := range items {
			fmt.Printf("%s\t%s\t%t\t%s\n", n.Metadata.Name, nodeReadyStatus(n), n.Spec.Unschedulable, nodeTaintsSummary(n))
		}
	case "nodetopology", "nodetopologies", "knt":
		items, err := kc.ListNodeTopologies(ctx)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(t model.NodeTopology) map[string]string { return t.Metadata.Labels })
		fmt.Printf("NAME\tNUMA\tCPUS\tDEVICES\tALLOCATABLE\tBINDING\n")
		for _, t := range items {
			cpus, allocatable := 0, 0
			for _, n := range t.Status.NUMANodes {
				if list, err := model.ParseCPUList(n.CPUs); err == nil {
					cpus += len(list)
				}
			}
			for _, d := range t.Status.Devices {
				if d.Allocatable {
					allocatable++
				}
			}
			fmt.Printf("%s\t%d\t%d\t%d\t%d\t%t\n", t.Metadata.Name, len(t.Status.NUMANodes), cpus, len(t.Status.Devices), allocatable, t.Status.NUMABinding)
		}
	case "acceleratorprofile", "acceleratorprofiles", "machineacceleratorprofile", "machineacceleratorprofiles", "kmap":
		items, err := kc.ListMachineAcceleratorProfiles(ctx)
		if err != nil {
			fatal(err)
		}
		items = selectorFilter(items, selector, func(p model.MachineAcceleratorProfile) map[string]string { return p.Metadata.Labels })
		fmt.Printf("NAME\tCLASS\tCOUNT\tPROFILE\tREADY\tCAPACITY\tMACHINES\n")
		for _, p := range items {
			class, count := p.Spec.Class, p.Spec.Count
			if class == "" {
				class = model.DeviceClassGPU
			}
			if count == 0 {
				count = 1
			}
			ready := "-"
			if c, ok := model.FindCondition(p.Status.Conditions, model.AcceleratorProfileReady); ok {
				ready = c.Status
			}
			fmt.Printf("%s\t%s\t%d\t%s\t%s\t%d\t%d\n", p.Metadata.Name, class, count, dash(p.Spec.Profile), ready, p.Status.Capacity, p.Status.Machines)
		}
	default:
		fatal(fmt.Errorf("unknown resource %q", resource))
	}
}

// cmdTop is kaironctl's kubectl-top-style view of live, cgroup-derived
// resource usage: `kaironctl top machines` prints each Machine's own
// Status.ResourceUsage (populated by internal/agent/agent.go straight from
// FluxVM's GET /v1/vms/{id}/stats -- see ResourceUsage's own doc comment),
// and `kaironctl top nodes` rolls those same per-Machine samples up by
// Spec.NodeName -- the fleet-wide, per-host hotspot view `kaironctl get
// machines` alone can't answer without an operator mentally grouping and
// summing rows themselves. Deliberately reuses exactly the data every
// Machine already reports rather than adding a new metrics pipeline: no new
// endpoint, no new controller-side aggregation, nothing to keep in sync --
// a rendering of state that already exists, the same "already-collected
// data, just not previously surfaced" shape as `kaironctl get nodes`
// itself.
//
// Like cmdGet, --selector k=v (repeatable) narrows which Machines count,
// via the exact same selectorFilter/model.LabelsMatch semantics; an empty
// selector means every Machine, never "none". "top machines" defaults to
// -n/--namespace exactly like "get machines" (cluster-wide only via an
// explicit selector isn't offered here, matching every other namespaced
// verb in this file); "top nodes" is cluster-wide by construction (a
// Machine's Spec.NodeName can span namespaces onto the same physical node),
// so -n/--namespace has no effect there, the same "ns silently unused for a
