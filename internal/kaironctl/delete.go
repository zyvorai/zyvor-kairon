// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdDelete(ctx context.Context, kc *kube.Client, args []string) {
	ns, args := nsFlag(args)
	if hasFlag(args, "selector") {
		cmdDeleteSelector(ctx, kc, ns, args)
		return
	}
	kind, name := resourceKindAndName("delete", args)
	canonical, err := deleteByKindName(ctx, kc, ns, kind, name)
	if err != nil {
		fatal(err)
	}
	okf("%s/%s deleted", canonical, name)
}

// hasFlag reports whether args contains --name or --name=value anywhere
// in the slice, used ahead of any flag.FlagSet parsing purely to pick
// which of delete's two calling conventions applies -- a single strings
// scan, not a full flag parse, since at this point args may still start
// with a bare KIND that isn't a flag at all.
func hasFlag(args []string, name string) bool {
	prefix := "--" + name
	for _, a := range args {
		if a == prefix || strings.HasPrefix(a, prefix+"=") {
			return true
		}
	}
	return false
}

// canonicalKind maps every alias cmdGet/cmdDescribe/cmdDelete/cmdEdit
// already recognize for one resource kind down to that kind's single
// canonical name (e.g. "vm"/"vms"/"machine"/"machines" -> "machine") --
// factored out of the old inline delete switch so both deleteByKindName
// and cmdDeleteSelector's --dry-run listing print the exact same
// canonical name a real delete would have used, without duplicating the
// alias list a second time.
func canonicalKind(kind string) (string, error) {
	switch kind {
	case "machine", "machines", "vm", "vms":
		return "machine", nil
	case "migration", "migrations", "machinemigrations":
		return "migration", nil
	case "snapshot", "snapshots", "machinesnapshots":
		return "snapshot", nil
	case "restore", "restores", "machinesnapshotrestores":
		return "restore", nil
	case "export", "exports", "machineexport", "machineexports":
		return "export", nil
	case "backup", "backups", "machinebackups":
		return "backup", nil
	case "backuprestore", "backuprestores", "machinebackuprestores":
		return "backuprestore", nil
	case "quota", "quotas", "machinequotas":
		return "quota", nil
	case "budget", "budgets", "machinedisruptionbudgets":
		return "budget", nil
	case "machineset", "machinesets":
		return "machineset", nil
	case "machinepool", "machinepools":
		return "machinepool", nil
	case "machineclaim", "machineclaims", "claim", "claims":
		return "machineclaim", nil
	case "instancetype", "instancetypes", "machineinstancetypes":
		return "instancetype", nil
	case "clusterinstancetype", "clusterinstancetypes", "machineclusterinstancetypes", "mcit":
		return "clusterinstancetype", nil
	case "preference", "preferences", "machinepreferences", "mpref":
		return "preference", nil
	case "clusterpreference", "clusterpreferences", "machineclusterpreferences", "mcpref":
		return "clusterpreference", nil
	case "migrationpolicy", "migrationpolicies":
		return "migrationpolicy", nil
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		return "snapshotschedule", nil
	case "backuppolicy", "backuppolicies", "machinebackuppolicies":
		return "backuppolicy", nil
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		return "networkpolicy", nil
	case "securitygroup", "securitygroups", "networksecuritygroups":
		return "securitygroup", nil
	case "virtualnetwork", "virtualnetworks", "vnet", "vnets":
		return "virtualnetwork", nil
	default:
		return "", fmt.Errorf("unknown resource %q", kind)
	}
}

// deleteByKindName is cmdDelete's original single-object delete dispatch,
// factored out so cmdDeleteSelector can call it once per label-matched
// name below without duplicating the kind switch a second time.
func deleteByKindName(ctx context.Context, kc *kube.Client, ns, kind, name string) (canonical string, err error) {
	canonical, err = canonicalKind(kind)
	if err != nil {
		return "", err
	}
	switch canonical {
	case "machine":
		return canonical, kc.DeleteMachine(ctx, ns, name)
	case "migration":
		return canonical, kc.DeleteMachineMigration(ctx, ns, name)
	case "snapshot":
		return canonical, kc.DeleteMachineSnapshot(ctx, ns, name)
	case "restore":
		return canonical, kc.DeleteMachineSnapshotRestore(ctx, ns, name)
	case "export":
		return canonical, kc.DeleteMachineExport(ctx, ns, name)
	case "backup":
		return canonical, kc.DeleteMachineBackup(ctx, ns, name)
	case "backuprestore":
		return canonical, kc.DeleteMachineBackupRestore(ctx, ns, name)
	case "quota":
		return canonical, kc.DeleteMachineQuota(ctx, ns, name)
	case "budget":
		return canonical, kc.DeleteMachineDisruptionBudget(ctx, ns, name)
	case "machineset":
		return canonical, kc.DeleteMachineSet(ctx, ns, name)
	case "machinepool":
		return canonical, kc.DeleteMachinePool(ctx, ns, name)
	case "machineclaim":
		return canonical, kc.DeleteMachineClaim(ctx, ns, name)
	case "instancetype":
		return canonical, kc.DeleteMachineInstanceType(ctx, ns, name)
	case "clusterinstancetype":
		return canonical, kc.DeleteMachineClusterInstanceType(ctx, name)
	case "preference":
		return canonical, kc.DeleteMachinePreference(ctx, ns, name)
	case "clusterpreference":
		return canonical, kc.DeleteMachineClusterPreference(ctx, name)
	case "migrationpolicy":
		return canonical, kc.DeleteMigrationPolicy(ctx, ns, name)
	case "snapshotschedule":
		return canonical, kc.DeleteMachineSnapshotSchedule(ctx, ns, name)
	case "backuppolicy":
		return canonical, kc.DeleteMachineBackupPolicy(ctx, ns, name)
	case "networkpolicy":
		return canonical, kc.DeleteMachineNetworkPolicy(ctx, ns, name)
	case "virtualnetwork":
		return canonical, kc.DeleteVirtualNetwork(ctx, ns, name)
	case "securitygroup":
		return canonical, kc.DeleteNetworkSecurityGroup(ctx, ns, name)
	default:
		return "", fmt.Errorf("unknown resource %q", kind) // unreachable: canonicalKind above already rejected anything else
	}
}

// appendIfMatch appends name to names when labels satisfies every
// key=value pair in selector, per model.LabelsMatch -- the same
// "matches" definition MachineDisruptionBudget.Spec.Selector,
// MachineNetworkPolicy.Spec.Selector and TopologySpreadConstraint's own
// LabelSelector already use, including LabelsMatch's own "an empty
// selector matches nothing, never everything" rule. cmdDeleteSelector
// never actually reaches matchingNames with an empty selector (it
// refuses first), but matchingNames stays correct on its own terms
// either way.
func appendIfMatch(names []string, name string, labels, selector map[string]string) []string {
	if model.LabelsMatch(labels, selector) {
		return append(names, name)
	}
	return names
}

// matchingNames lists every existing resource of kind in ns and returns
// the metadata.name of each one whose labels satisfy selector (see
// appendIfMatch). One case per kind, mirroring cmdGet's own per-kind
// switch, since each kind has its own List*Namespace call and item type.
func matchingNames(ctx context.Context, kc *kube.Client, ns, kind string, selector map[string]string) ([]string, error) {
	var names []string
	switch kind {
	case "machine", "machines", "vm", "vms":
		items, err := kc.ListMachinesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "migration", "migrations", "machinemigrations":
		items, err := kc.ListMachineMigrationsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "snapshot", "snapshots", "machinesnapshots":
		items, err := kc.ListMachineSnapshotsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "restore", "restores", "machinesnapshotrestores":
		items, err := kc.ListMachineSnapshotRestoresNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "export", "exports", "machineexport", "machineexports":
		items, err := kc.ListMachineExportsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "backup", "backups", "machinebackups":
		items, err := kc.ListMachineBackupsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "backuprestore", "backuprestores", "machinebackuprestores":
		items, err := kc.ListMachineBackupRestoresNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "quota", "quotas", "machinequotas":
		items, err := kc.ListMachineQuotasNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "budget", "budgets", "machinedisruptionbudgets":
		items, err := kc.ListMachineDisruptionBudgetsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "machineset", "machinesets":
		items, err := kc.ListMachineSetsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "machinepool", "machinepools":
		items, err := kc.ListMachinePoolsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "machineclaim", "machineclaims", "claim", "claims":
		items, err := kc.ListMachineClaimsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "instancetype", "instancetypes", "machineinstancetypes":
		items, err := kc.ListMachineInstanceTypesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "migrationpolicy", "migrationpolicies":
		items, err := kc.ListMigrationPoliciesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		items, err := kc.ListMachineSnapshotSchedulesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "virtualnetwork", "virtualnetworks", "vnet", "vnets":
		items, err := kc.ListVirtualNetworksNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "backuppolicy", "backuppolicies", "machinebackuppolicies":
		items, err := kc.ListMachineBackupPoliciesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		items, err := kc.ListMachineNetworkPoliciesNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	case "securitygroup", "securitygroups", "networksecuritygroups":
		items, err := kc.ListNetworkSecurityGroupsNamespace(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			names = appendIfMatch(names, it.Metadata.Name, it.Metadata.Labels, selector)
		}
	default:
		return nil, fmt.Errorf("unknown resource %q", kind)
	}
	return names, nil
}

// cmdDeleteSelector implements `delete RESOURCE --selector k=v [--selector
// k2=v2] [--dry-run]`: RESOURCE is required and explicit (no "1 arg
// defaults to machine" convention here, unlike resourceKindAndName --
// bulk deletion is exactly the place a wrong default is most dangerous),
// and --selector is required and must resolve to a non-empty map. That
// last rule is deliberate and load-bearing: model.LabelsMatch already
// treats an empty selector as matching nothing, so without this check a
// mistyped `--selector` (or one that parses to an empty map for some
// other reason) would silently no-op instead of surfacing the mistake --
// `--dry-run` is not itself the safety mechanism, both together are. Every
// deletion happens one object at a time via the same deleteByKindName
// cmdDelete's single-object path already uses, so a bulk delete leaves
// exactly the same audit trail (individual DELETE calls, individual
// finalizer/reconcile handling) as running `delete NAME` in a loop by
// hand -- there is no separate bulk-delete API call to reason about.
func cmdDeleteSelector(ctx context.Context, kc *kube.Client, ns string, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl delete RESOURCE --selector k=v [--selector k2=v2] [--dry-run]"))
	}
	kind := strings.ToLower(args[0])
	canonical, err := canonicalKind(kind)
	if err != nil {
		fatal(err)
	}
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "label key=value every matching resource must carry (repeatable -- every pair must match; required and must be non-empty)")
	dryRun := fs.Bool("dry-run", false, "print what would be deleted without deleting anything")
	_ = fs.Parse(args[1:])
	sel, err := parseKeyValues(selector)
	if err != nil {
		fatal(err)
	}
	if len(sel) == 0 {
		fatal(fmt.Errorf("--selector is required and must be non-empty for bulk delete (usage: kaironctl delete %s --selector k=v) -- an empty selector matches nothing, by design, rather than risk being misread as \"everything\"", kind))
	}
	names, err := matchingNames(ctx, kc, ns, kind, sel)
	if err != nil {
		fatal(err)
	}
	if len(names) == 0 {
		fmt.Println("no resources matched selector; nothing to delete")
		return
	}
	sort.Strings(names) // deterministic order for a repeatable dry-run/real-run diff
	for _, name := range names {
		if *dryRun {
			okf("%s/%s (dry-run, not deleted)", canonical, name)
			continue
		}
		if _, err := deleteByKindName(ctx, kc, ns, kind, name); err != nil {
			fatal(fmt.Errorf("deleting %s/%s: %w", canonical, name, err))
		}
		okf("%s/%s deleted", canonical, name)
	}
}

// cmdFence is the operator-attested recovery action for a Machine whose
// node kairon-controller has detected as unreachable (see
// internal/controller/fencing.go's detectUnreachableNodes) -- the same
// "park it, wait for an attested human decision" shape as `recover` uses
// for a NeedsRecovery migration, applied to a dead node instead of an
// ambiguous migration commit. Kairon cannot itself confirm a node is
// truly gone rather than just unreachable, so this refuses to run at all
// unless status already shows NodeUnreachable=True, and --reason is
// required as the operator's own attestation (e.g. "confirmed powered off
// via iDRAC at 14:02" or "node deleted from the cluster") -- if the old
// node comes back while its FluxVM runtime is still actually running,
// fencing it anyway risks the exact split-brain NeedsRecovery exists to
// prevent for migrations. Clears spec.nodeName (picked up by the normal
// scheduling loop on the next tick, same as a brand-new Machine) and every
// status field tied to the old runtime, so the new node treats this as a
