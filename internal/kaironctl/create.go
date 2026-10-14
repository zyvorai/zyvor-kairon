// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func machineSpecFromFlags(fs *flag.FlagSet) (spec func() model.MachineSpec, image *string) {
	image = fs.String("image", "", "FluxVM host-local image path")
	cpu := fs.String("cpu", "2", "vCPU quantity")
	memory := fs.String("memory", "2Gi", "memory quantity")
	backend := fs.String("backend", "qemu", "qemu|cloud-hypervisor|firecracker|flux-vm|auto")
	network := fs.String("network", "user", "user|tap|macvtap")
	netns := fs.Bool("netns", false, "use per-VM network namespace for TAP")
	var forwards stringSliceFlag
	fs.Var(&forwards, "forward", "host port forward hostPort:guestPort[/proto], e.g. 2222:22 (repeatable; mode=user only)")
	hostname := fs.String("hostname", "", "guest hostname to set via cloud-init")
	guestUser := fs.String("user", "", "guest username to configure via cloud-init")
	var sshKeys stringSliceFlag
	fs.Var(&sshKeys, "ssh-key", "SSH public key to authorize in the guest via cloud-init (repeatable)")
	var packages stringSliceFlag
	fs.Var(&packages, "package", "package to install via cloud-init at first boot (repeatable)")
	var runcmd stringSliceFlag
	fs.Var(&runcmd, "runcmd", "shell command to run via cloud-init at first boot (repeatable)")
	guestAgent := fs.Bool("guest-agent", false, "attach a qemu-guest-agent channel (the image must run qemu-guest-agent); reports status.guestIP and enables guest exec")
	priority := fs.Int("priority", 0, "scheduling priority: when a reconcile tick can't fit every pending Machine (node capacity or MachineQuota), higher values are attempted first; default 0, negative values are valid for a below-default class")
	return func() model.MachineSpec {
		pf, err := parseForwards(forwards)
		if err != nil {
			fatal(err)
		}
		return model.MachineSpec{
			Image:     model.ImageSpec{Path: *image},
			Resources: model.ResourceSpec{CPU: *cpu, Memory: *memory},
			Runtime:   model.RuntimeSpec{Backend: *backend},
			Network:   model.NetworkSpec{Mode: *network, NetNS: *netns, Forwards: pf},
			CloudInit: model.CloudInitSpec{
				Hostname:          *hostname,
				User:              *guestUser,
				SSHAuthorizedKeys: sshKeys,
				Packages:          packages,
				RunCmd:            runcmd,
			},
			GuestAgent: model.GuestAgentSpec{Enabled: *guestAgent},
			PowerState: "Running",
			Priority:   int32(*priority),
		}
	}, image
}

// cmdCreate handles both `kaironctl create NAME --image PATH [flags]` (a
// Machine, its original and unchanged form) and, dispatched by keyword,
// `create machineset|instancetype|migrationpolicy NAME [flags]`.
// "machineset"/"instancetype"/"migrationpolicy" are recognized as a KIND
// only by exact match against this fixed alias list, never by any other
// heuristic -- the same tradeoff resourceKindAndName's own doc comment
// already accepts for get/describe/delete: a Machine actually named e.g.
// "machineset" can't be created through this bare form (kubectl apply is
// the escape hatch, as it already is for every field these flags don't
// cover). Every other NAME falls straight through to the original
// create-a-Machine behavior below, byte-for-byte unchanged.
func cmdCreate(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create NAME --image PATH [flags] | create machineset|machinepool|instancetype|migrationpolicy|snapshotschedule|quota|budget|networkpolicy|securitygroup NAME [flags]"))
	}
	switch strings.ToLower(args[0]) {
	case "machineset", "machinesets":
		cmdCreateMachineSet(ctx, kc, args[1:])
		return
	case "machinepool", "machinepools":
		cmdCreateMachinePool(ctx, kc, args[1:])
		return
	case "instancetype", "instancetypes", "machineinstancetypes":
		cmdCreateInstanceType(ctx, kc, args[1:])
		return
	case "migrationpolicy", "migrationpolicies":
		cmdCreateMigrationPolicy(ctx, kc, args[1:])
		return
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		cmdCreateSnapshotSchedule(ctx, kc, args[1:])
		return
	case "quota", "quotas", "machinequotas":
		cmdCreateQuota(ctx, kc, args[1:])
		return
	case "budget", "budgets", "machinedisruptionbudgets":
		cmdCreateBudget(ctx, kc, args[1:])
		return
	case "networkpolicy", "networkpolicies", "machinenetworkpolicies":
		cmdCreateNetworkPolicy(ctx, kc, args[1:])
		return
	case "securitygroup", "securitygroups", "networksecuritygroups":
		cmdCreateSecurityGroup(ctx, kc, args[1:])
		return
	}
	name := args[0]
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	specFn, image := machineSpecFromFlags(fs)
	_ = fs.Parse(args[1:])
	if *image == "" {
		fatal(fmt.Errorf("--image PATH is required"))
	}
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec:     specFn(),
	}
	out, err := kc.CreateMachine(ctx, *ns, m)
	if err != nil {
		fatal(err)
	}
	okf("machine/%s created", out.Metadata.Name)
}

// cmdCreateMachineSet handles `kaironctl create machineset NAME [flags]`,
// dispatched from cmdCreate. Reuses machineSpecFromFlags for
// Template.Spec so a MachineSet's per-replica spec is built exactly the
// same way a plain Machine's is.
func cmdCreateMachineSet(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create machineset NAME --image PATH [--replicas N] [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create machineset", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	replicas := fs.Int("replicas", 1, "desired Machine count")
	strategy := fs.String("strategy", "", "RollingUpdate (default) | Recreate")
	maxUnavailable := fs.String("max-unavailable", "", "integer or percentage bound on simultaneously-missing/outdated replicas during RollingUpdate; empty defaults to 1")
	var labels stringSliceFlag
	fs.Var(&labels, "label", "label key=value applied to every replica this MachineSet creates, in addition to its own bookkeeping labels (repeatable)")
	specFn, image := machineSpecFromFlags(fs)
	_ = fs.Parse(args[1:])
	if *image == "" {
		fatal(fmt.Errorf("--image PATH is required"))
	}
	labelMap, err := parseKeyValues(labels)
	if err != nil {
		fatal(err)
	}
	ms := model.MachineSet{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSet},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec: model.MachineSetSpec{
			Replicas:       *replicas,
			Template:       model.MachineTemplate{Labels: labelMap, Spec: specFn()},
			Strategy:       *strategy,
			MaxUnavailable: *maxUnavailable,
		},
	}
	out, err := kc.CreateMachineSet(ctx, *ns, ms)
	if err != nil {
		fatal(err)
	}
	okf("machineset/%s created", out.Metadata.Name)
}

// cmdCreateMachinePool handles `kaironctl create machinepool NAME --image
// PATH [--replicas N] [flags]`; the template flags are the same as create.
func cmdCreateMachinePool(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create machinepool NAME --image PATH [--replicas N] [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create machinepool", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	replicas := fs.Int("replicas", 1, "warm (unclaimed) Machines to keep booted")
	var labels stringSliceFlag
	fs.Var(&labels, "label", "label key=value applied to every pool member (repeatable)")
	specFn, image := machineSpecFromFlags(fs)
	_ = fs.Parse(args[1:])
	if *image == "" {
		fatal(fmt.Errorf("--image PATH is required"))
	}
	labelMap, err := parseKeyValues(labels)
	if err != nil {
		fatal(err)
	}
	pool := model.MachinePool{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachinePool},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec: model.MachinePoolSpec{
			Replicas: *replicas,
			Template: model.MachineTemplate{Labels: labelMap, Spec: specFn()},
		},
	}
	out, err := kc.CreateMachinePool(ctx, *ns, pool)
	if err != nil {
		fatal(err)
	}
	okf("machinepool/%s created", out.Metadata.Name)
}

// cmdCreateInstanceType handles `kaironctl create instancetype NAME --cpu N
// --memory SIZE [flags]`, dispatched from cmdCreate. A strict subset of
// cmdCreate's own resource flags -- MachineInstanceTypeSpec is just a
// ResourceSpec, nothing else to map.
func cmdCreateInstanceType(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create instancetype NAME --cpu N --memory SIZE [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create instancetype", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	cpu := fs.String("cpu", "", "vCPU quantity (required)")
	memory := fs.String("memory", "", "memory quantity (required)")
	maxCPU := fs.String("max-cpu", "", "hotplug headroom vCPU ceiling")
	maxMemory := fs.String("max-memory", "", "hotplug headroom memory ceiling")
	hugepages := fs.Bool("hugepages", false, "back guest memory with hugepages (qemu only)")
	numaNode := fs.Int("numa-node", -1, "pin to a specific host NUMA node (qemu only); -1 leaves it unset")
	cpuSet := fs.String("cpu-set", "", "guest-visible vNUMA CPUSet hint (qemu only)")
	cpuPinning := fs.Bool("cpu-pinning", false, "real, exclusive host-core allocation (qemu only)")
	_ = fs.Parse(args[1:])
	if *cpu == "" || *memory == "" {
		fatal(fmt.Errorf("--cpu and --memory are both required"))
	}
	resources := model.ResourceSpec{
		CPU: *cpu, Memory: *memory, MaxCPU: *maxCPU, MaxMemory: *maxMemory,
		Hugepages: *hugepages, CPUSet: *cpuSet, CPUPinning: *cpuPinning,
	}
	if *numaNode >= 0 {
		resources.NUMANode = numaNode
	}
	it := model.MachineInstanceType{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineInstanceType},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec:     model.MachineInstanceTypeSpec{Resources: resources},
	}
	out, err := kc.CreateMachineInstanceType(ctx, *ns, it)
	if err != nil {
		fatal(err)
	}
	okf("instancetype/%s created", out.Metadata.Name)
}

// cmdCreateMigrationPolicy handles `kaironctl create migrationpolicy NAME
// --selector k=v [flags]`, dispatched from cmdCreate.
func cmdCreateMigrationPolicy(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create migrationpolicy NAME --selector k=v [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create migrationpolicy", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "label key=value this policy applies to (repeatable, required)")
	bandwidth := fs.Uint64("bandwidth-mbps", 0, "default migration bandwidth for a matching Machine's migration, if it didn't already set one explicitly")
	maxConcurrent := fs.Int("max-concurrent", 0, "cap on simultaneous non-terminal migrations of matching Machines cluster-wide; 0 is unlimited within this policy's own scope")
	var tuning policyTuningFlags
	tuning.register(fs)
	_ = fs.Parse(args[1:])
	if len(selector) == 0 {
		fatal(fmt.Errorf("--selector k=v is required (repeatable)"))
	}
	selectorMap, err := parseKeyValues(selector)
	if err != nil {
		fatal(err)
	}
	p := model.MigrationPolicy{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMigrationPolicy},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec:     model.MigrationPolicySpec{Selector: selectorMap, BandwidthMbps: *bandwidth, MaxConcurrent: *maxConcurrent},
	}
	tuningPatch, err := tuning.patch(visited(fs))
	if err != nil {
		fatal(err)
	}
	raw, _ := json.Marshal(tuningPatch)
	if err := json.Unmarshal(raw, &p.Spec); err != nil {
		fatal(err)
	}
	out, err := kc.CreateMigrationPolicy(ctx, *ns, p)
	if err != nil {
		fatal(err)
	}
	okf("migrationpolicy/%s created", out.Metadata.Name)
}

// cmdCreateSnapshotSchedule handles `kaironctl create snapshotschedule NAME
// --selector k=v --interval-seconds N [flags]`, dispatched from cmdCreate.
func cmdCreateSnapshotSchedule(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create snapshotschedule NAME --selector k=v (--interval-seconds N | --daily-at HH:MM) [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create snapshotschedule", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "label key=value a Machine must match to be snapshotted (repeatable, required)")
	intervalSeconds := fs.Int("interval-seconds", 0, "minimum seconds between runs (minimum 60; this or --daily-at is required)")
	dailyAt := fs.String("daily-at", "", "fire once a day at HH:MM UTC instead of every --interval-seconds")
	jitterSeconds := fs.Int("jitter-seconds", 0, "spread runs by a stable per-schedule offset in [0, N) seconds")
	maxAgeSeconds := fs.Int("max-age-seconds", 0, "also prune this schedule's snapshots older than N seconds (the newest is always kept)")
	volumeSnapshotClassName := fs.String("volume-snapshot-class", "", "VolumeSnapshotClassName passed through to every MachineSnapshot this schedule creates")
	suspend := fs.Bool("suspend", false, "create the schedule already suspended")
	keepLast := fs.Int("keep-last", 0, "retain only the N most recent ready-to-use snapshots this schedule created per Machine, deleting older ones (0, the default, never prunes)")
	startingDeadlineSeconds := fs.Int("starting-deadline-seconds", 0, "skip (rather than immediately fire) a run found more than this many seconds late, e.g. after the controller was down (0, the default, never skips -- an overdue run always fires)")
	_ = fs.Parse(args[1:])
	if len(selector) == 0 {
		fatal(fmt.Errorf("--selector k=v is required (repeatable)"))
	}
	selectorMap, err := parseKeyValues(selector)
	if err != nil {
		fatal(err)
	}
	s := model.MachineSnapshotSchedule{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshotSchedule},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec: model.MachineSnapshotScheduleSpec{
			Selector:                selectorMap,
			IntervalSeconds:         *intervalSeconds,
			VolumeSnapshotClassName: *volumeSnapshotClassName,
			Suspend:                 *suspend,
			KeepLast:                *keepLast,
			StartingDeadlineSeconds: *startingDeadlineSeconds,
			DailyAt:                 *dailyAt,
			JitterSeconds:           *jitterSeconds,
			MaxAgeSeconds:           *maxAgeSeconds,
		},
	}
	if err := s.Spec.Validate(); err != nil {
		fatal(err)
	}
	out, err := kc.CreateMachineSnapshotSchedule(ctx, *ns, s)
	if err != nil {
		fatal(err)
	}
	okf("snapshotschedule/%s created", out.Metadata.Name)
}

// cmdCreateQuota handles `kaironctl create quota NAME [--max-machines N]
// [--max-total-cpu N] [--max-total-memory SIZE]`, dispatched from cmdCreate.
// Unlike migrationpolicy/snapshotschedule's selector, MachineQuotaSpec has
// no required field the CRD itself enforces (every dimension is optional --
// see internal/model/machinequota.go's own doc comment), but a MachineQuota
// with every dimension unset caps nothing at all, so this still refuses to
// create one -- the same "don't let an operator create a resource that
// provably does nothing" instinct as requiring --selector elsewhere, just
// enforced client-side here since the CRD schema can't express "at least
// one of these three."
func cmdCreateQuota(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create quota NAME [--max-machines N] [--max-total-cpu N] [--max-total-memory SIZE]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create quota", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	maxMachines := fs.Int("max-machines", -1, "cap on scheduled Machine count in this namespace; -1 (the default) leaves it unset (no cap on this dimension)")
	maxTotalCPU := fs.String("max-total-cpu", "", "cap on total vCPUs scheduled in this namespace, summed across every scheduled Machine's spec.resources.cpu")
	maxTotalMemory := fs.String("max-total-memory", "", "cap on total memory scheduled in this namespace, summed across every scheduled Machine's spec.resources.memory")
	_ = fs.Parse(args[1:])
	if *maxMachines < 0 && *maxTotalCPU == "" && *maxTotalMemory == "" {
		fatal(fmt.Errorf("at least one of --max-machines, --max-total-cpu, --max-total-memory is required -- a quota with no dimension set caps nothing"))
	}
	spec := model.MachineQuotaSpec{MaxTotalCPU: *maxTotalCPU, MaxTotalMemory: *maxTotalMemory}
	if *maxMachines >= 0 {
		spec.MaxMachines = maxMachines
	}
	q := model.MachineQuota{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineQuota},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec:     spec,
	}
	out, err := kc.CreateMachineQuota(ctx, *ns, q)
	if err != nil {
		fatal(err)
	}
	okf("quota/%s created", out.Metadata.Name)
}

// cmdCreateBudget handles `kaironctl create budget NAME --selector k=v
// (--min-available X | --max-unavailable X)`, dispatched from cmdCreate.
// Requires exactly one of --min-available/--max-unavailable, mirroring
// MachineDisruptionBudgetSpec.DesiredHealthy's own documented contract
// (internal/model/disruption.go) -- a budget created with both or neither
// set would parse fine against the CRD schema (which doesn't express
// "exactly one of," the same limit --max-machines above works around) but
// would then fail DesiredHealthy on every reconcile tick and every
// `evacuate` check, so this catches the mistake up front instead of
// shipping a silently-broken budget.
func cmdCreateBudget(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create budget NAME --selector k=v (--min-available X | --max-unavailable X)"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create budget", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "label key=value a Machine must match to count against this budget (repeatable, required)")
	minAvailable := fs.String("min-available", "", "integer or \"N%\" floor on healthy matching Machines (exactly one of this and --max-unavailable is required)")
	maxUnavailable := fs.String("max-unavailable", "", "integer or \"N%\" ceiling on unhealthy/disrupted matching Machines (exactly one of this and --min-available is required)")
	_ = fs.Parse(args[1:])
	if len(selector) == 0 {
		fatal(fmt.Errorf("--selector k=v is required (repeatable)"))
	}
	if (*minAvailable == "") == (*maxUnavailable == "") {
		fatal(fmt.Errorf("exactly one of --min-available or --max-unavailable is required, not both or neither"))
	}
	selectorMap, err := parseKeyValues(selector)
	if err != nil {
		fatal(err)
	}
	b := model.MachineDisruptionBudget{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineDisruptionBudget},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: selectorMap, MinAvailable: *minAvailable, MaxUnavailable: *maxUnavailable},
	}
	out, err := kc.CreateMachineDisruptionBudget(ctx, *ns, b)
	if err != nil {
		fatal(err)
	}
	okf("budget/%s created", out.Metadata.Name)
}

// vmNetworkPolicyFromFlags registers every model.VmNetworkPolicy field as a
// flag on fs and returns a closure that builds the struct from whatever was
// parsed -- shared between cmdCreateNetworkPolicy and cmdCreateSecurityGroup
// since both CRDs embed the exact same nested policy shape (see
// model.VmNetworkPolicy's own doc comment: "mirrors FluxVM VmNetworkPolicy").
// spec.cnp (MachineNetworkPolicy-only, a free-form FluxVM CiliumNetworkPolicy
// document) is deliberately not exposed here -- same "stays create-time-only
// through kubectl/YAML, not a CLI flag" treatment cmdEditMachine already
// gives every Machine-spec field this CLI doesn't expose flags for; an
// arbitrary nested JSON document has no sensible flag shape anyway.
func vmNetworkPolicyFromFlags(fs *flag.FlagSet) func() model.VmNetworkPolicy {
	defaultAllow := fs.Bool("default-allow", false, "allow everything by default; allow/deny-cidr and allow-port then narrow that instead of building up allowlists from a default-deny baseline")
	var allowCidrs, denyCidrs, allowPorts, allowFqdns, policyGroups, policyLabels, entities stringSliceFlag
	fs.Var(&allowCidrs, "allow-cidr", "destination CIDR to allow, e.g. 10.0.0.0/8 (repeatable)")
	fs.Var(&denyCidrs, "deny-cidr", "destination CIDR to deny (repeatable)")
	fs.Var(&allowPorts, "allow-port", "proto/port rule to allow, e.g. tcp/443 or udp/53 (repeatable)")
	fs.Var(&allowFqdns, "allow-fqdn", "FQDN to allow, resolved by FluxVM at apply time (repeatable)")
	fs.Var(&policyGroups, "policy-group", "NetworkSecurityGroup name whose membership this policy inherits (repeatable)")
	fs.Var(&policyLabels, "policy-label", "key=value tag this policy matches NetworkSecurityGroup membership against, e.g. tier=frontend (repeatable)")
	fs.Var(&entities, "entity", "well-known FluxVM entity to allow, e.g. world/cluster (repeatable)")
	maxEgressMbps := fs.Uint64("max-egress-mbps", 0, "cap egress bandwidth in Mbps; 0 (the default) leaves it uncapped")
	maxEgressPps := fs.Uint64("max-egress-pps", 0, "cap egress packet rate in packets/sec; 0 (the default) leaves it uncapped")
	auditMode := fs.Bool("audit-mode", false, "log traffic that would be denied instead of dropping it")
	allowIcmp := fs.Bool("allow-icmp", false, "permit ICMP/ICMPv6 regardless of --allow-port")
	sampleRate := fs.Uint("sample-rate", 0, "flow-log sampling rate; 0 (the default) means no sampling")
	return func() model.VmNetworkPolicy {
		p := model.VmNetworkPolicy{
			DefaultAllow: *defaultAllow,
			AllowCidrs:   []string(allowCidrs),
			DenyCidrs:    []string(denyCidrs),
			AllowPorts:   []string(allowPorts),
			AllowFqdns:   []string(allowFqdns),
			Groups:       []string(policyGroups),
			Labels:       []string(policyLabels),
			Entities:     []string(entities),
			AuditMode:    *auditMode,
			AllowIcmp:    *allowIcmp,
			SampleRate:   uint32(*sampleRate),
		}
		if *maxEgressMbps > 0 {
			v := uint32(*maxEgressMbps)
			p.MaxEgressMbps = &v
		}
		if *maxEgressPps > 0 {
			v := uint32(*maxEgressPps)
			p.MaxEgressPps = &v
		}
		return p
	}
}

// cmdCreateNetworkPolicy handles `kaironctl create networkpolicy NAME
// (--machine-name X | --selector k=v) [policy flags]`, dispatched from
// cmdCreate -- closing the same "no kaironctl create for this CRD at all"
// gap cmdCreateSecurityGroup closes for its sibling. Requires at least one
// of --machine-name/--selector, the same "don't create an object that
// provably matches nothing" instinct as cmdCreateQuota's own dimension
// check: MachineNetworkPolicySpec.MachineName/Selector are both optional at
// the CRD-schema level, but a policy with neither set matches no Machine at
// all (model.LabelsMatch's own doc comment: an empty selector never
// matches), the same silently-does-nothing shape this project has
// consistently refused to let `create` produce elsewhere.
func cmdCreateNetworkPolicy(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create networkpolicy NAME (--machine-name X | --selector k=v) [--allow-cidr CIDR] [--deny-cidr CIDR] [--allow-port proto/port] [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create networkpolicy", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	machineName := fs.String("machine-name", "", "single Machine (same namespace) this policy targets; takes precedence over --selector")
	var selector stringSliceFlag
	fs.Var(&selector, "selector", "label key=value a Machine must match when --machine-name is unset (repeatable)")
	policyFn := vmNetworkPolicyFromFlags(fs)
	_ = fs.Parse(args[1:])
	if *machineName == "" && len(selector) == 0 {
		fatal(fmt.Errorf("at least one of --machine-name or --selector is required -- a policy with neither set matches no Machine"))
	}
	selectorMap, err := parseKeyValues(selector)
	if err != nil {
		fatal(err)
	}
	p := model.MachineNetworkPolicy{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineNetworkPolicy},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec: model.MachineNetworkPolicySpec{
			MachineName: *machineName,
			Selector:    selectorMap,
			Policy:      policyFn(),
		},
	}
	out, err := kc.CreateMachineNetworkPolicy(ctx, *ns, p)
	if err != nil {
		fatal(err)
	}
	okf("networkpolicy/%s created", out.Metadata.Name)
}

// cmdCreateSecurityGroup handles `kaironctl create securitygroup NAME
// [--group-name X] [--priority N] [--description TEXT] [policy flags]`,
// dispatched from cmdCreate. Unlike MachineNetworkPolicy, NetworkSecurityGroup
// has no required targeting field -- it's a named, reusable group other
// policies opt into via their own --policy-group -- so an empty --group-name
// is fine (Spec.GroupName defaults to metadata.name, see
// NetworkSecurityGroup.FluxGroupName) and there is no "matches nothing"
// failure mode to guard against here.
func cmdCreateSecurityGroup(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl create securitygroup NAME [--group-name X] [--priority N] [--description TEXT] [--allow-cidr CIDR] [flags]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("create securitygroup", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	groupName := fs.String("group-name", "", "FluxVM group name; defaults to NAME when unset")
	priority := fs.Uint("priority", 0, "lower value wins on a rate/deny tie against another group")
	description := fs.String("description", "", "human-readable note carried through to FluxVM")
	var groupLabels stringSliceFlag
	fs.Var(&groupLabels, "group-label", "key=value tag FluxVM matches group membership against, e.g. tier=frontend (repeatable)")
	policyFn := vmNetworkPolicyFromFlags(fs)
	_ = fs.Parse(args[1:])
	g := model.NetworkSecurityGroup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindNetworkSecurityGroup},
		Metadata: model.ObjectMeta{Name: name, Namespace: *ns},
		Spec: model.NetworkSecurityGroupSpec{
			GroupName:   *groupName,
			Labels:      []string(groupLabels),
			Priority:    uint32(*priority),
			Description: *description,
			Policy:      policyFn(),
		},
	}
	out, err := kc.CreateNetworkSecurityGroup(ctx, *ns, g)
	if err != nil {
		fatal(err)
	}
	okf("securitygroup/%s created", out.Metadata.Name)
}

// cmdScale mutates spec.replicas on an existing MachineSet -- the only
// resource kind this verb supports for a first cut, since no other kind
// kaironctl manages has a sensible "scale" operation. Takes its own
// lightweight KIND NAME split (not resourceKindAndName, which is shaped
// for get/describe/delete's "1 arg defaults to machine" convention and
// doesn't fit a verb that always requires an explicit KIND plus trailing
// flags). Once --selector appears anywhere after KIND, dispatches to
// cmdScaleSelector for the bulk "same replica count across every match"
// form instead of expecting a single trailing NAME -- see
// cmdScaleSelector's own doc comment for why scale (unlike edit) is a
