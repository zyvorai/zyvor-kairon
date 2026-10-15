// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/oteltrace"
	"github.com/zyvorai/kairon/internal/scheduler"
)

type Controller struct {
	Kube  *kube.Client
	Fleet *fleet.Engine
	// Cosign, when set, makes the webhook verify the image signature of
	// every agent-pool Machine. Nil keeps the annotation shape check only.
	Cosign ImageVerifier
	// Attest verifies confidential-compute reports. Nil leaves every
	// confidential Machine unsealed.
	Attest AttestationVerifier
	// AttestationWriters may change attestation-verified and
	// attestation-nonce through the webhook. Empty allows any
	// kairon-controller service account.
	AttestationWriters []string
	Scheduler          scheduler.Scheduler
	Log                *slog.Logger
	// Metrics, when set, observes every MachineMigration once per
	// reconcile tick. Optional -- nil-checked, same convention as
	// Agent.MigrationPeer.
	Metrics *metrics.Recorder
	// Tracer, when Endpoint is set, emits opt-in OTLP/HTTP reconcile spans
	// (internal/oteltrace). Nil or empty endpoint is a no-op.
	Tracer *oteltrace.Tracer
	// MaxConcurrentPerNode/MaxConcurrentCluster cap how many non-terminal
	// migrations may touch a single node / run cluster-wide at once. 0 (the
	// default) is unlimited -- today's unchanged behavior. A migration that
	// would exceed either limit is rejected into the existing Blocked phase
	// (see isActiveMigrationPhase/migrationLoad), the same mechanism used
	// for an unschedulable Machine or an ineligible target.
	MaxConcurrentPerNode int
	MaxConcurrentCluster int
	// CordonEvacuation, when Enabled, automatically migrates Machines off
	// a Node whose spec.unschedulable transitions to true -- see
	// internal/controller/cordon.go. Zero value (Enabled: false) is
	// today's unchanged behavior: cordoning a node does nothing to
	// already-running Machines.
	CordonEvacuation CordonEvacuation
	// MemoryEvacuation, when Enabled, migrates Machines off nodes whose
	// KaironMemoryPressure condition stays True -- see memoryevac.go.
	MemoryEvacuation MemoryPressureEvacuation
	// StaleEvacuation, when Enabled, fences opted-in Machines off nodes
	// attested dead -- see internal/controller/staleevac.go.
	StaleEvacuation StaleEvacuation
	// Maintenance drives node maintenance mode -- see maintenance.go.
	Maintenance NodeMaintenance
	// DRS rebalances Machines across nodes -- see drs.go.
	DRS DRS
	// DR runs MachineProtectionPlans -- see protection.go.
	DR             DisasterRecovery
	drsLastRun     time.Time
	drsLastPlan    string
	staleEvacMu    sync.Mutex
	staleEvacNoted map[string]bool
	exposeMu       sync.Mutex
	exposeWarned   bool
	// CiliumAttach enables reconcile of CiliumExternalWorkload for Machines
	// with spec.network.ciliumAttach. Off by default (Helm network.ciliumAttach).
	CiliumAttach bool
	// CiliumPolicySync enables MachineNetworkPolicy → CiliumNetworkPolicy
	// sync when spec.cilium.sync is true. Off by default.
	CiliumPolicySync bool
	// NodeLivenessLeaseNamespace, when set, feeds kairon-node liveness
	// Leases into detectUnreachableNodes (Ready + stale lease →
	// NodeUnreachable). Empty keeps Node-Ready-only detection.
	NodeLivenessLeaseNamespace string
	// Atlas provisions spec.volumes[].atlas through the Atlas storage
	// gateway (internal/controller/atlas.go). Zero value disables it.
	Atlas AtlasConfig
}

// isActiveMigrationPhase reports whether a migration in this phase is
// currently consuming node resources -- true for everything except the
// not-yet-admitted "" / Pending and the terminal phases.
func isActiveMigrationPhase(phase string) bool {
	switch phase {
	case "Starting", "Running", "Cutover", "Adopting", "Stopping", "Restarting", "NeedsRecovery":
		return true
	}
	return false
}

// migrationLoad is a snapshot of how many active migrations currently touch
// each node / the cluster as a whole, computed once per Reconcile tick.
type migrationLoad struct {
	byNode map[string]int
	total  int
	// byMachine maps "namespace/machine" to the active migration touching
	// it ("name (phase)"). A Machine has one migration in flight at a time:
	// a second one would race the first over the same runtime and disk.
	byMachine map[string]string
}

func machineKey(namespace, machine string) string { return namespace + "/" + machine }

func newMigrationLoad(migrations []model.MachineMigration) *migrationLoad {
	l := &migrationLoad{byNode: map[string]int{}, byMachine: map[string]string{}}
	for _, m := range migrations {
		if !isActiveMigrationPhase(m.Status.Phase) {
			continue
		}
		if _, ok := l.byMachine[machineKey(m.Namespace(), m.Spec.MachineName)]; !ok {
			l.byMachine[machineKey(m.Namespace(), m.Spec.MachineName)] = fmt.Sprintf("%s (%s)", m.Metadata.Name, m.Status.Phase)
		}
		l.total++
		if m.Status.SourceNode != "" {
			l.byNode[m.Status.SourceNode]++
		}
		if m.Status.TargetNode != "" && m.Status.TargetNode != m.Status.SourceNode {
			l.byNode[m.Status.TargetNode]++
		}
	}
	return l
}

// admit records a newly-admitted migration against the load snapshot, so a
// burst of Pending migrations processed in the same tick (e.g. one
// `kaironctl evacuate` creating several at once) can't all pass a stale
// pre-tick count.
func (l *migrationLoad) admit(sourceNode, targetNode string) {
	l.total++
	if sourceNode != "" {
		l.byNode[sourceNode]++
	}
	if targetNode != "" && targetNode != sourceNode {
		l.byNode[targetNode]++
	}
}

func (c *Controller) Reconcile(ctx context.Context) error {
	machines, err := c.Kube.ListMachines(ctx)
	if err != nil {
		return err
	}
	c.syncAssignedNodeLabels(ctx, machines)
	c.reconcileTenantFences(ctx, machines)
	machines = c.resolveInstanceTypes(ctx, machines)
	machines, imagesNotReady := c.resolveImageRefs(ctx, machines)
	acceleratorProfiles := c.listAcceleratorProfiles(ctx)
	machines, acceleratorsNotReady := c.resolveAccelerators(ctx, machines, acceleratorProfiles)
	machines, volumesNotReady := c.reconcileAtlasVolumes(ctx, machines)
	c.reconcileCiliumAttach(ctx, machines)
	c.reconcileCiliumPolicySync(ctx)
	c.reconcileExposedServices(ctx, machines)
	c.reconcileLifecycle(ctx, machines, time.Now())
	nodes, err := c.Kube.ListNodes(ctx)
	if err != nil {
		return err
	}
	nodeLoad := scheduler.BuildNodeLoad(machines)
	agentSessions, err := c.listAgentSessions(ctx)
	if err != nil {
		return err
	}
	reserveAgentSessions(nodeLoad, agentSessions)
	machineIndex := indexMachines(machines)

	migrations, err := c.Kube.ListMachineMigrations(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	c.syncMigrationSourceLabels(ctx, migrations)
	if c.Metrics != nil {
		c.Metrics.ObserveMigrations(migrations)
	}
	migLoad := newMigrationLoad(migrations)
	policyStates := c.loadMigrationPolicyStates(ctx, machines, migrations)
	for _, migration := range migrations {
		span := c.Tracer.Start(ctx, "reconcile.migration", map[string]string{
			"kind": "migration", "namespace": migration.Namespace(), "name": migration.Metadata.Name,
		})
		err := c.reconcileMigration(ctx, migration, machineIndex, machines, nodes, nodeLoad, migLoad, policyStates)
		span.End(err)
		if err != nil {
			status := migration.Status
			status.Phase = "Failed"
			status.Message = err.Error()
			c.Log.Error("migration reconcile failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("migration")
			}
			if statusErr := c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status); statusErr != nil {
				c.Log.Error("migration status patch failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", statusErr)
			}
		}
	}
	c.patchMigrationPolicyStatuses(ctx, policyStates)

	snapshots, err := c.Kube.ListMachineSnapshots(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	for _, snapshot := range snapshots {
		span := c.Tracer.Start(ctx, "reconcile.snapshot", map[string]string{
			"kind": "snapshot", "namespace": snapshot.Namespace(), "name": snapshot.Metadata.Name,
		})
		err := c.reconcileSnapshot(ctx, snapshot, machineIndex)
		span.End(err)
		if err != nil {
			status := snapshot.Status
			status.Phase = "Failed"
			status.ReadyToUse = false
			status.Message = err.Error()
			c.Log.Error("snapshot reconcile failed", "namespace", snapshot.Namespace(), "snapshot", snapshot.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("snapshot")
			}
			if statusErr := c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status); statusErr != nil {
				c.Log.Error("snapshot status patch failed", "namespace", snapshot.Namespace(), "snapshot", snapshot.Metadata.Name, "error", statusErr)
			}
		}
	}

	restores, err := c.Kube.ListMachineSnapshotRestores(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	for _, restore := range restores {
		span := c.Tracer.Start(ctx, "reconcile.snapshotrestore", map[string]string{
			"kind": "snapshotrestore", "namespace": restore.Namespace(), "name": restore.Metadata.Name,
		})
		err := c.reconcileSnapshotRestore(ctx, restore)
		span.End(err)
		if err != nil {
			status := restore.Status
			status.Phase = "Failed"
			status.Message = err.Error()
			c.Log.Error("snapshot restore reconcile failed", "namespace", restore.Namespace(), "restore", restore.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("snapshotrestore")
			}
			if statusErr := c.Kube.PatchMachineSnapshotRestoreStatus(ctx, restore.Namespace(), restore.Metadata.Name, status); statusErr != nil {
				c.Log.Error("snapshot restore status patch failed", "namespace", restore.Namespace(), "restore", restore.Metadata.Name, "error", statusErr)
			}
		}
	}

	if err := c.reconcileMachineBackups(ctx, machineIndex); err != nil {
		return err
	}

	machineSets, err := c.Kube.ListMachineSets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	c.reconcileMachineSets(ctx, machineSets, machines)
	c.reconcileVirtualNetworks(ctx, machines, nodes)
	macNetworks := c.reconcileMacNetworks(ctx, nodes)

	claims, err := c.Kube.ListMachineClaims(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	bound := c.reconcileMachineClaims(ctx, claims, machines)
	pools, err := c.Kube.ListMachinePools(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	c.reconcileMachinePools(ctx, pools, machines, bound)

	quotas, err := c.Kube.ListMachineQuotas(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	quotaTrackers, err := BuildQuotaTrackers(quotas, machines)
	if err != nil {
		return err
	}
	seedQuotaWithAgentSessions(quotaTrackers, agentSessions)
	draPlacements, err := c.buildDRAPlacements(ctx, machines)
	if err != nil {
		return err
	}

	// pending collects every Machine this tick will actually try to
	// schedule -- the same "nothing to schedule" skips as before this
	// existed (deleted, already assigned, or desired Stopped/Halted with
	// no runtime yet to stop) -- then SortByPriorityDesc reorders it so a
	// burst of pending Machines that exceeds available node capacity or
	// MachineQuota headroom admits the highest spec.Priority ones first
	// within this same tick, instead of whichever the API happened to
	// list first. machines itself (the full list, scheduled and pending
	// alike) is untouched and still what Choose/admitQuota consult for
	// affinity/topology/quota-usage context below -- only the outer
	// iteration order changes.
	var pending []model.Machine
	for _, m := range machines {
		desired := m.DesiredPowerState()
		if m.Metadata.DeletionTimestamp != nil || m.Spec.NodeName != "" || model.PowerStateFreesCapacity(desired) {
			continue
		}
		if _, waiting := volumesNotReady[m.Namespace()+"/"+m.Metadata.Name]; waiting {
			continue
		}
		if _, waiting := imagesNotReady[m.Namespace()+"/"+m.Metadata.Name]; waiting {
			continue
		}
		if _, waiting := acceleratorsNotReady[m.Namespace()+"/"+m.Metadata.Name]; waiting {
			continue
		}
		pending = append(pending, m)
	}
	scheduler.SortByPriorityDesc(pending)
	topologies, err := c.nodeTopologies(ctx, nodes, machines)
	if err != nil {
		return err
	}
	c.reconcileHibernatedResumes(ctx, machines, nodes, nodeLoad, topologies)
	c.reconcileAcceleratorProfiles(ctx, acceleratorProfiles, machines, nodes, topologies)

	var unschedulable []model.Machine
	for _, m := range pending {
		key := m.Namespace() + "/" + m.Metadata.Name
		topo := scheduler.TopologyInput{Nodes: topologies, DRA: draPlacements[key], MacNetworks: macNetworks}
		node, err := c.Scheduler.ChooseTopology(m, nodes, machines, nodeLoad, topo)
		if err != nil {
			unschedulable = append(unschedulable, m)
			reason := unschedulableReason(m, err)
			c.observeTopologyFailure(m, reason)
			status := m.Status
			status.Phase = "Pending"
			status.Message = err.Error()
			status.Conditions = model.SetCondition(status.Conditions, model.Condition{
				Type: "Scheduled", Status: "False", Reason: reason, Message: err.Error(),
			})
			if statusErr := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); statusErr != nil {
				c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", statusErr)
			}
			continue
		}
		// Quota is checked (and spent) only once a node is otherwise
		// eligible -- checking it earlier would falsely reserve capacity
		// for a Machine that turns out to have no eligible node anyway,
		// starving a later Machine in this same pass that could have fit.
		if blocker := admitQuota(quotaTrackers, m); blocker != "" {
			unschedulable = append(unschedulable, m)
			status := m.Status
			status.Phase = "Pending"
			status.Message = blocker
			status.Conditions = model.SetCondition(status.Conditions, model.Condition{
				Type: "Scheduled", Status: "False", Reason: "QuotaBlocked", Message: blocker,
			})
			if statusErr := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); statusErr != nil {
				c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", statusErr)
			}
			// Same blocker string already patched into status.Message
			// above, also recorded as a real Event -- purely observing a
			// decision already made, no new logic -- so it shows up in
			// `kubectl describe machine`'s Events tab too, not just a
			// status field an operator has to already know to read. Best
			// effort: a failure here never blocks scheduling or retries
			// this tick's status patch, it's just logged.
			if evErr := c.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, "QuotaBlocked", blocker, "Warning"); evErr != nil {
				c.Log.Error("machine quota-blocked event record failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", evErr)
			}
			continue
		}
		specPatch := map[string]any{"nodeName": node}
		var chosenNode model.Node
		for _, n := range nodes {
			if n.Metadata.Name == node {
				chosenNode = n
				break
			}
		}
		var cpuset []uint32
		var topoAlloc *model.TopologyAllocation
		if m.Spec.NeedsTopology() {
			// Same snapshot ChooseTopology filtered on, so node, cores,
			// NUMA memory and devices land in one patch below.
			alloc, _, reason, ok := scheduler.AllocateTopology(m, chosenNode, machines, topo)
			if ok && reason != "" {
				msg := fmt.Sprintf("%s: %s", scheduler.TopologyUnsatisfiable, reason)
				c.observeTopologyFailure(m, scheduler.TopologyUnsatisfiable)
				status := m.Status
				status.Phase = "Pending"
				status.Message = msg
				status.Conditions = model.SetCondition(status.Conditions, model.Condition{
					Type: "Scheduled", Status: "False", Reason: scheduler.TopologyUnsatisfiable, Message: msg,
				})
				if statusErr := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); statusErr != nil {
					c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", statusErr)
				}
				unschedulable = append(unschedulable, m)
				continue
			}
			if ok {
				topoAlloc = alloc.TopologyAllocation()
				specPatch["topologyAllocation"] = topoAlloc
				if len(alloc.CPUs) > 0 {
					cpuset = alloc.CPUs
					specPatch["resources"] = map[string]any{"allocatedCpuSet": cpuset}
				}
			}
		}
		if m.Spec.Resources.CPUPinning && cpuset == nil {
			// Allocated in the same scheduling pass Choose's own
			// eligibility check already used (the same machines/nodes
			// snapshot), so "which node" and "which cores" are decided
			// together and patched in one call below -- they can never
			// land separately. Topology Machines take their cores from
			// AllocateTopology above instead.
			var err error
			cpuset, err = scheduler.AllocateCPUSet(m, chosenNode, machines)
			if err != nil {
				status := m.Status
				status.Phase = "Pending"
				status.Message = err.Error()
				status.Conditions = model.SetCondition(status.Conditions, model.Condition{
					Type: "Scheduled", Status: "False", Reason: "CPUPinningFailed", Message: err.Error(),
				})
				if statusErr := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); statusErr != nil {
					c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", statusErr)
				}
				unschedulable = append(unschedulable, m)
				continue
			}
			specPatch["resources"] = map[string]any{"allocatedCpuSet": cpuset}
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"spec":     specPatch,
			"metadata": map[string]any{"labels": map[string]any{model.AssignedNodeLabel: node}},
		}); err != nil {
			return err
		}
		scheduler.Reserve(nodeLoad, node, m)
		markScheduled(machines, m, node, cpuset, topoAlloc)
		c.Log.Info("scheduled machine", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node)
	}
	c.reconcileAgentSessions(ctx, agentSessions, nodes, machines, nodeLoad, quotaTrackers)
	c.preemptForPending(ctx, unschedulable, machines, migrations)

	// observedQuotas carries each tracker's final Spec/Status pair (seeded
	// usage plus whatever this same tick's scheduling loop admitted) --
	// exactly the same values PatchMachineQuotaStatus below writes to the
	// real object, so ObserveQuotas never reports numbers that disagree
	// with what `kubectl get machinequota` would show for the same tick.
	observedQuotas := make([]model.MachineQuota, 0, len(quotas))
	for _, list := range quotaTrackers {
		for _, t := range list {
			q := t.quota
			q.Status = t.used
			observedQuotas = append(observedQuotas, q)
			if statusErr := c.Kube.PatchMachineQuotaStatus(ctx, t.quota.Namespace(), t.quota.Metadata.Name, t.used); statusErr != nil {
				c.Log.Error("machine quota status patch failed", "namespace", t.quota.Namespace(), "quota", t.quota.Metadata.Name, "error", statusErr)
			}
		}
	}
	if c.Metrics != nil {
		c.Metrics.ObserveQuotas(observedQuotas)
	}

	if err := c.reconcileDisruptionBudgetsStatus(ctx, machines, migrations); err != nil {
		c.Log.Error("machine disruption budget status computation failed", "error", err)
	}

	if err := c.reconcileMachineSnapshotSchedules(ctx, machines); err != nil {
		c.Log.Error("machine snapshot schedule reconciliation failed", "error", err)
	}

	if err := c.reconcileMachineBackupPolicies(ctx, machines); err != nil {
		c.Log.Error("machine backup policy reconciliation failed", "error", err)
	}
	if err := c.reconcileProtectionPlans(ctx, machines); err != nil {
		c.Log.Error("machine protection plan reconciliation failed", "error", err)
	}

	c.reconcileAttestation(ctx, machines, nodes)
	c.reconcileAgentStatus(ctx, machines, nodes)
	c.reconcileNodeMaintenance(ctx, machines, nodes, migrations, nodeLoad)
	c.reconcileCordonEvacuation(ctx, machines, nodes, migrations)
	c.reconcileMemoryPressureEvacuation(ctx, machines, nodes, migrations)
	c.reconcileDRS(ctx, machines, nodes, migrations)
	c.detectUnreachableNodes(ctx, machines, nodes)
	c.reconcileStaleEvacuation(ctx, machines, nodes, migrations)
	if err := c.reconcileFleet(ctx, machines, nodes, migrations); err != nil {
		c.Log.Error("fleet automation blocked", "error", err)
	}
	return nil
}

func countAssigned(machines []model.Machine) map[string]int {
	assigned := map[string]int{}
	for _, m := range machines {
		// Running and Paused both keep a real FluxVM runtime (and its
		// resident RAM) alive on the node -- only they count here. A
		// Paused Machine's guest CPUs are suspended, but the process, and
		// the host memory backing it, are not. Stopped (full teardown)
		// and Halted (VMM process terminated, FluxVM's own record kept)
		// both genuinely free the capacity they held, so neither counts.
		desired := m.DesiredPowerState()
		if m.Spec.NodeName != "" && m.Metadata.DeletionTimestamp == nil && (desired == "Running" || desired == "Paused") {
			assigned[m.Spec.NodeName]++
		}
	}
	return assigned
}

func indexMachines(machines []model.Machine) map[string]model.Machine {
	out := make(map[string]model.Machine, len(machines))
	for _, m := range machines {
		out[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	return out
}

func (c *Controller) reconcileMigration(ctx context.Context, migration model.MachineMigration, machines map[string]model.Machine, machineList []model.Machine, nodes []model.Node, nodeLoad map[string]scheduler.NodeLoad, load *migrationLoad, policyStates []*MigrationPolicyState) error {
	if migration.Status.Phase == "Succeeded" || migration.Status.Phase == "Failed" || migration.Status.Phase == "Blocked" || migration.Status.Phase == "NeedsRecovery" || migration.Status.Phase == "Cancelled" {
		return nil
	}
	if strings.TrimSpace(migration.Spec.MachineName) == "" {
		return fmt.Errorf("spec.machineName is required")
	}
	machine, ok := machines[migration.Namespace()+"/"+migration.Spec.MachineName]
	if !ok {
		return fmt.Errorf("machine %s/%s not found", migration.Namespace(), migration.Spec.MachineName)
	}
	if machine.Metadata.DeletionTimestamp != nil {
		return fmt.Errorf("machine is being deleted")
	}

	status := migration.Status
	phase := status.Phase
	if phase == "" || phase == "Pending" {
		if machine.Spec.NodeName == "" {
			return c.blockMigration(ctx, migration, "Machine has not been scheduled yet")
		}
		if other, busy := load.byMachine[machineKey(migration.Namespace(), migration.Spec.MachineName)]; busy {
			return c.blockMigration(ctx, migration, "another migration of this Machine is in progress: "+other+"; wait for it to finish, or resolve it if it is NeedsRecovery")
		}
		if c.MaxConcurrentCluster > 0 && load.total >= c.MaxConcurrentCluster {
			return c.blockMigration(ctx, migration, fmt.Sprintf("cluster migration concurrency limit reached (%d active, max %d)", load.total, c.MaxConcurrentCluster))
		}
		if c.MaxConcurrentPerNode > 0 && load.byNode[machine.Spec.NodeName] >= c.MaxConcurrentPerNode {
			return c.blockMigration(ctx, migration, fmt.Sprintf("source node %s has reached its concurrent migration limit (%d active, max %d)", machine.Spec.NodeName, load.byNode[machine.Spec.NodeName], c.MaxConcurrentPerNode))
		}
		if blocker := AdmitMigrationPolicy(policyStates, machine); blocker != "" {
			return c.blockMigration(ctx, migration, blocker)
		}
		strategy, err := effectiveStrategy(machine, migration)
		if err != nil {
			return c.blockMigration(ctx, migration, err.Error())
		}
		target, err := c.migrationTarget(ctx, machine, migration.Spec.TargetNode, strategy, nodes, machineList, nodeLoad)
		if err != nil {
			return c.blockMigration(ctx, migration, err.Error())
		}
		if strategy == "live" && machine.Spec.NeedsTopology() {
			alloc, err := c.liveTopologyTarget(ctx, machine, target, nodes, machineList)
			if err != nil {
				return c.blockMigration(ctx, migration, err.Error())
			}
			if alloc != nil {
				status.TargetTopologyAllocation = alloc.TopologyAllocation()
				status.TargetAllocatedCPUSet = alloc.CPUs
			}
		}
		if c.MaxConcurrentPerNode > 0 && load.byNode[target] >= c.MaxConcurrentPerNode {
			return c.blockMigration(ctx, migration, fmt.Sprintf("target node %s has reached its concurrent migration limit (%d active, max %d)", target, load.byNode[target], c.MaxConcurrentPerNode))
		}
		tuning := MigrationTuning{}
		if strategy == "live" {
			tuning = ResolveMigrationTuning(policyStates, machine)
		} else {
			tuning.BandwidthMbps = BandwidthMbpsFromPolicies(policyStates, machine)
		}
		patch, err := MigrationTuningPatch(migration.Spec, tuning)
		if err != nil {
			return c.blockMigration(ctx, migration, err.Error())
		}
		if patch != nil {
			if err := c.Kube.PatchMachineMigration(ctx, migration.Namespace(), migration.Metadata.Name, map[string]any{"spec": patch}); err != nil {
				c.Log.Error("migration policy tuning snapshot patch failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", err)
			}
		}
		status.SourceNode = machine.Spec.NodeName
		status.TargetNode = target
		status.EffectiveStrategy = strategy
		status.Message = ""
		load.admit(status.SourceNode, status.TargetNode)
		load.byMachine[machineKey(migration.Namespace(), migration.Spec.MachineName)] = fmt.Sprintf("%s (%s)", migration.Metadata.Name, "Starting")
		if strategy == "live" {
			status.Phase = "Starting"
			status.Message = "source node agent will securely prepare the target before touching the source runtime"
			if err := c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status); err != nil {
				return err
			}
			if err := c.ensureMigrationSourceLabel(ctx, migration, status.SourceNode); err != nil {
				c.Log.Error("migration source-node label stamp failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", err)
			}
			return nil
		}
		status.Phase = "Stopping"
		status.Message = "stopping source runtime for controlled cold evacuation"
		if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, map[string]any{"spec": map[string]any{"powerState": "Stopped"}}); err != nil {
			return err
		}
		if err := c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status); err != nil {
			return err
		}
		if err := c.ensureMigrationSourceLabel(ctx, migration, status.SourceNode); err != nil {
			c.Log.Error("migration source-node label stamp failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", err)
		}
		return nil
	}

	switch phase {
	case "Starting", "Running":
		// The source node agent owns peer preparation, transfer and commit.
		return nil
	case "Cutover":
		if status.EffectiveStrategy != "live" {
			return fmt.Errorf("cutover phase is only valid for live migration")
		}
		spec := map[string]any{"nodeName": status.TargetNode}
		if status.TargetTopologyAllocation != nil {
			spec["topologyAllocation"] = status.TargetTopologyAllocation
			spec["resources"] = map[string]any{"allocatedCpuSet": status.TargetAllocatedCPUSet}
		}
		patch := map[string]any{
			"spec": spec,
			"metadata": map[string]any{
				"annotations": map[string]any{
					model.AnnotationAdoptOnly:    "true",
					model.AnnotationMigrationRef: migration.Metadata.Name,
				},
				"labels": map[string]any{model.AssignedNodeLabel: status.TargetNode},
			},
		}
		if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, patch); err != nil {
			return err
		}
		status.Phase = "Adopting"
		status.Message = "source migration completed; target is in adopt-only mode"
		return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
	case "Adopting":
		if machine.Spec.NodeName != status.TargetNode || machine.Status.NodeName != status.TargetNode || machine.Status.Phase != "Running" {
			return nil
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
			model.AnnotationAdoptOnly:    nil,
			model.AnnotationMigrationRef: nil,
		}}}
		if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, patch); err != nil {
			return err
		}
		status.Phase = "Succeeded"
		status.Message = "live migration completed and target runtime adopted"
		return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
	case "Stopping":
		if machine.Status.Phase != "Stopped" {
			return nil
		}
		spec := map[string]any{"nodeName": status.TargetNode, "powerState": "Running"}
		if machine.Spec.NeedsTopology() {
			if err := c.coldTopologyTarget(ctx, machine, status.TargetNode, nodes, machineList, spec); err != nil {
				status.Message = err.Error()
				return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
			}
		}
		if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, map[string]any{
			"spec":     spec,
			"metadata": map[string]any{"labels": map[string]any{model.AssignedNodeLabel: status.TargetNode}},
		}); err != nil {
			return err
		}
		status.Phase = "Restarting"
		status.Message = "source stopped; Machine reassigned to target"
		return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
	case "Restarting":
		if machine.Spec.NodeName == status.TargetNode && machine.Status.NodeName == status.TargetNode && machine.Status.Phase == "Running" {
			status.Phase = "Succeeded"
			status.Message = "cold migration completed"
			return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
		}
		return nil
	default:
		return fmt.Errorf("unknown migration status phase %q", phase)
	}
}

func (c *Controller) blockMigration(ctx context.Context, migration model.MachineMigration, message string) error {
	status := migration.Status
	status.Phase = "Blocked"
	status.Message = message
	return c.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status)
}

func effectiveStrategy(machine model.Machine, migration model.MachineMigration) (string, error) {
	strategy := strings.ToLower(strings.TrimSpace(migration.Spec.Strategy))
	if strategy == "" {
		strategy = "auto"
	}
	switch strategy {
	case "cold":
		return "cold", nil
	case "live":
		if !liveBackendEligible(machine.Spec.Runtime.Backend) {
			return "", fmt.Errorf("live migration requires qemu backend; Machine requests %q", machine.Spec.Runtime.Backend)
		}
		return "live", nil
	case "auto":
		// Until Kairon has cluster-wide backend capability discovery, auto stays
		// conservative and chooses the fully implemented cold path. Operators can
		// explicitly request live to use the v0.3 secure handshake.
		return "cold", nil
	default:
		return "", fmt.Errorf("unsupported migration strategy %q", migration.Spec.Strategy)
	}
}

func liveBackendEligible(backend string) bool {
	return backend == "" || backend == "auto" || backend == "qemu"
}

func (c *Controller) migrationTarget(ctx context.Context, machine model.Machine, requested, strategy string, nodes []model.Node, machineList []model.Machine, nodeLoad map[string]scheduler.NodeLoad) (string, error) {
	var candidates []model.Node
	for _, n := range nodes {
		if n.Metadata.Name == machine.Spec.NodeName {
			continue
		}
		if requested != "" && n.Metadata.Name != requested {
			continue
		}
		candidates = append(candidates, n)
	}
	if requested != "" && len(candidates) == 0 {
		return "", fmt.Errorf("target node %q does not exist or is the current source node", requested)
	}
	// No DRA hint here: a Machine with deviceClaims migrating to a
	// different node would leave its VFIO passthrough device behind on the
	// source node anyway (device claims aren't re-resolved by a
	// migration), so a DRA topology hint has nothing meaningful to nudge
	// toward for target selection specifically.
	var target string
	var err error
	if machine.Spec.NeedsTopology() {
		if strategy == "live" && len(machine.Spec.Devices) > 0 {
			return "", fmt.Errorf("migration preflight failed: spec.devices are passed through with VFIO and cannot live-migrate; use strategy cold")
		}
		topologies, terr := c.nodeTopologies(ctx, nodes, machineList)
		if terr != nil {
			return "", fmt.Errorf("list node topologies: %w", terr)
		}
		target, err = c.Scheduler.ChooseTopology(machine, candidates, machineList, nodeLoad, scheduler.TopologyInput{Nodes: topologies})
	} else {
		target, err = c.Scheduler.Choose(machine, candidates, machineList, nodeLoad, "")
	}
	if err != nil {
		if requested != "" {
			return "", fmt.Errorf("target node %q is not eligible: %w", requested, err)
		}
		return "", fmt.Errorf("no migration target available: %w", err)
	}
	// Preflight against the chosen candidate only -- not a fallback search
	// through the rest of candidates if it fails. See migrationPreflight's
	// own doc comment for what this does and doesn't check.
	if sourceNode, ok := nodeByName(nodes, machine.Spec.NodeName); ok {
		if targetNode, ok := nodeByName(nodes, target); ok {
			if blocker := migrationPreflight(sourceNode, targetNode); blocker != "" {
				return "", fmt.Errorf("migration preflight failed: %s", blocker)
			}
			if blocker := deviceClaimsPreflight(machine, targetNode, strategy); blocker != "" {
				return "", fmt.Errorf("migration preflight failed: %s", blocker)
			}
		}
	}
	return target, nil
}

func nodeByName(nodes []model.Node, name string) (model.Node, bool) {
	for _, n := range nodes {
		if n.Metadata.Name == name {
			return n, true
		}
	}
	return model.Node{}, false
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

func snapshotVolumeName(snapshot, volume string) string {
	name := strings.ToLower(snapshot + "-" + volume)
	name = nonDNS.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if name == "" {
		return "kairon-snapshot"
	}
	return name
}

func (c *Controller) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := time.Now()
		err := c.Reconcile(ctx)
		if c.Metrics != nil {
			c.Metrics.ObserveReconcile(time.Since(start), err)
		}
		if err != nil {
			c.Log.Error("reconcile failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
