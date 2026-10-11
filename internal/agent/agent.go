// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/hotplugpersist"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/nodeliveness"
	"github.com/zyvorai/kairon/internal/oteltrace"
)

var pciBDFPattern = regexp.MustCompile(`(?i)^(?:[0-9a-f]{4}:)?[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

type Agent struct {
	NodeName       string
	Kube           *kube.Client
	Flux           *fluxvm.Client
	DefaultBackend string
	ImageRoot      string
	VFIOAllowlist  map[string]struct{}
	MigrationPeer  *migration.Client
	// Restores is shared with the destination migration server. Nil skips
	// conntrack result projection.
	Restores         *migration.RestoreStore
	SourceMigrator   migration.SourceDriver
	MigrationPort    int
	MigrationPeerURL func(context.Context, string) (string, error)
	Log              *slog.Logger
	// Metrics, when set, observes reconcile loop iteration duration/errors
	// (see Run) and, if Kube.Observe is wired to the same Recorder,
	// apiserver call health too. Optional, nil-checked, same convention as
	// MigrationPeer.
	Metrics *metrics.Recorder
	// edge caches the last policy list and drop counts for the VM edge.
	edge edgeCache
	// tenantDenies is namespace + NUL + tenant to the fence group's
	// deny CIDRs, refreshed at the start of reconcileNetworkResources.
	// Nil means no fence was listed; policy apply then posts spec as-is.
	tenantDenies map[string][]string
	// edgeEvents suppresses repeated edge Warning Events per Machine.
	edgeEvents edgeEventDedup
	// edgeBaseline scores each Machine's traffic against its own history.
	edgeBaseline edgeBaselineState
	// EdgeBaselinePath persists edgeBaseline across restarts; empty keeps
	// it in memory only.
	EdgeBaselinePath string
	// attest rate-limits capability labeling and guest report fetches.
	attest attestState
	// SysRoot is where confidential capability is read; empty is /sys.
	SysRoot string
	// CPUManagerState is kubelet's cpu_manager_state; empty uses the default path.
	CPUManagerState string
	// ReservedCPUs is the cpuset kept off the discovered pinnable set.
	// Empty refuses discovery, so no label is published.
	ReservedCPUs string
	pin          pinState
	// AttestCommand is the in-guest command that prints a base64 report
	// for {kind} and hex {data}; empty uses the go-sev-guest/go-tdx-guest
	// attest tool.
	AttestCommand string
	// Tracer, when Endpoint is set, emits opt-in OTLP/HTTP reconcile spans.
	Tracer *oteltrace.Tracer
	// CSISocketPath/CSIStagingDir/CSIPublishDir configure network-block
	// (CSI-backed) PersistentVolume support -- see internal/agent/csi.go
	// and internal/csinode. CSISocketPath empty (the default) means a
	// CSI-backed spec.volumes[0] is refused with a clear error rather
	// than silently failing; hostPath/local-backed volumes and plain
	// spec.image.path are entirely unaffected either way.
	CSISocketPath string
	CSIStagingDir string
	CSIPublishDir string
	// CSIChapSecretNamespace, when set, allows resolveCSIVolume to read a
	// PV's nodeStageSecretRef for CHAP -- but only Secrets in this exact
	// namespace. Empty (the default) refuses any secret ref, matching the
	// deliberate "no cluster-wide Secret get" posture documented in
	// SECURITY.md. Helm's node.csi.chap.enabled turns this on together
	// with a namespaced Role granting secrets get in the release namespace.
	CSIChapSecretNamespace string
	// ImageCacheDir configures spec.image.source support -- see
	// internal/agent/imageimport.go. Empty (the default) means a Machine
	// setting spec.image.source is refused with a clear error rather than
	// silently failing; spec.image.path and spec.volumes are entirely
	// unaffected either way. Same fail-closed convention as
	// CSISocketPath above.
	ImageCacheDir string
	// RegistryHTTP is the client spec.image.source.oci pulls use; nil
	// means http.DefaultClient.
	RegistryHTTP *http.Client
	// ThirdPartyCSIDrivers maps a CSI driver name (e.g.
	// "rbd.csi.ceph.com") to the Unix socket path kairon-node should dial
	// as its own generic CSI client for a spec.volumes[0] PV naming that
	// driver -- an explicit, operator-configured allowlist, never
	// automatic /var/lib/kubelet/plugins_registry/ discovery. See
	// docs/guides/machine-storage-thirdparty-csi.md for the scope this
	// covers. Empty/nil (the default) means a PV naming any driver other
	// than Kairon's own is still refused outright, exactly as before this
	// existed.
	ThirdPartyCSIDrivers map[string]string
	// ThirdPartyCSISecretNamespace is the only namespace a third-party PV's
	// nodeStageSecretRef / nodePublishSecretRef may point into. Empty
	// refuses any secret ref.
	ThirdPartyCSISecretNamespace string
	// LivenessLeaseNamespace, when set, makes Run renew this node's own
	// coordination.k8s.io/v1 Lease (internal/nodeliveness) once per
	// reconcile tick -- a real "I'm alive and reconciling" signal
	// kaironctl fence cross-checks against Node Ready before proceeding.
	// Empty (the default) disables this entirely: no Lease writes, no new
	// RBAC required, exactly kairon-node's behavior before this existed.
	// Same opt-in-via-namespace convention kairon-controller's own
	// leader election already established (KAIRON_CONTROLLER_NAMESPACE).
	LivenessLeaseNamespace string
	// LivenessLeaseDuration overrides nodeliveness.DefaultLeaseDuration
	// when non-zero.
	LivenessLeaseDuration time.Duration
	// NetworkDefaultDeny, when true, makes reconcileNetworkResources
	// (network.go) push DefaultAllow: false onto every Machine on this
	// node that isn't currently matched by any MachineNetworkPolicy/
	// NetworkSecurityGroup-derived policy -- closing the gap where such a
	// Machine is never touched at all and silently keeps FluxVM's native
	// defaultAllow: true. False (the default) is today's unchanged
	// behavior: an unmatched Machine is left alone. This is a coarse,
	// global toggle, not namespace-aware isolation -- turning it on with
	// no policies written yet cuts all VM-to-VM connectivity outright, the
	// same disruptive-if-flipped-blind posture webhook.enabled already
	// has, hence off by default. See docs/guides/network-policy.md.
	NetworkDefaultDeny bool
	// AtlasRBDPools lists the Ceph pools this node may boot Atlas rbd-mode
	// volumes from. Empty refuses atlas.mode=rbd.
	AtlasRBDPools []string
	// csiConn caches the dialed connection to kairon-csi-node's local
	// Unix socket -- see csiNodeClient in csi.go. Safe unguarded for the
	// same reason guestIPCheckedAt below is: Reconcile only ever runs
	// single-goroutine, sequential.
	csiConn *grpc.ClientConn
	// thirdPartyCSIConns caches dialed connections to third-party CSI
	// driver sockets, keyed by driver name -- the same caching reasoning
	// csiConn has, just one connection per configured driver instead of a
	// single fixed one.
	thirdPartyCSIConns map[string]*grpc.ClientConn
	// guestIPCheckedAt tracks, per "namespace/name", the last time
	// projectNetworkStatus actually queried the guest agent for a Machine
	// that already has a resolved guestIP -- see guestAgentRecheckInterval
	// in network.go. Safe unguarded: Reconcile (and therefore every call
	// into projectNetworkStatus) runs machine-by-machine inside a single
	// goroutine, never concurrently (see Run's ticker loop below).
	guestIPCheckedAt map[string]time.Time
	// backups tracks MachineBackup/MachineBackupRestore disk copies
	// running in their own goroutines (see backup.go).
	backups backupJobs
}

func (a *Agent) Reconcile(ctx context.Context) error {
	selector := model.AssignedNodeLabelSelector(a.NodeName)
	machines, err := a.Kube.ListMachinesWithSelector(ctx, selector)
	if err != nil {
		return err
	}
	activeEdgePrefixes := make(map[string]bool, len(machines))
	for _, m := range machines {
		if m.Spec.NodeName == a.NodeName {
			activeEdgePrefixes[m.Namespace()+"/"+m.Metadata.Name+"/"] = true
		}
	}
	a.edge.drops.Prune(activeEdgePrefixes)
	for _, m := range machines {
		// Defense in depth: never act on a Machine whose spec disagrees
		// with the assignment label (stale label / partial patch).
		if m.Spec.NodeName != a.NodeName {
			continue
		}
		span := a.Tracer.Start(ctx, "reconcile.machine", map[string]string{
			"kind": "machine", "namespace": m.Namespace(), "name": m.Metadata.Name,
		})
		err := a.reconcileMachine(ctx, m)
		span.End(err)
		if err != nil {
			a.Log.Error("machine reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			if a.Metrics != nil {
				a.Metrics.ObserveReconcileItemError("machine")
			}
			status := m.Status
			status.Phase = "Error"
			status.NodeName = a.NodeName
			status.Message = err.Error()
			status.Conditions = model.SetCondition(status.Conditions, model.Condition{
				Type: "Ready", Status: "False", Reason: "ReconcileFailed", Message: err.Error(),
			})
			if statusErr := a.patchMachineStatusIfChanged(ctx, m, status); statusErr != nil {
				a.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", statusErr)
			}
		}
	}
	if err := a.reconcileNetworkResources(ctx); err != nil {
		return err
	}
	a.publishConfidentialCapability(ctx)
	a.publishPinnableCPUs(ctx)
	local := make(map[string]model.Machine, len(machines))
	for _, m := range machines {
		if m.Spec.NodeName == a.NodeName {
			local[m.Namespace()+"/"+m.Metadata.Name] = m
			a.serveAttestation(ctx, m)
		}
	}
	if err := a.reconcileBackups(ctx, local); err != nil {
		a.Log.Error("machine backup reconcile failed", "error", err)
	}
	migSelector := model.MigrationSourceNodeLabelSelector(a.NodeName)
	migrations, err := a.Kube.ListMachineMigrationsWithSelector(ctx, migSelector)
	if err != nil {
		// This lets a v0.2 node binary coexist during a rolling CRD upgrade.
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, migration := range migrations {
		if migration.Status.SourceNode != a.NodeName {
			continue
		}
		span := a.Tracer.Start(ctx, "reconcile.migration", map[string]string{
			"kind": "migration", "namespace": migration.Namespace(), "name": migration.Metadata.Name,
		})
		err := a.reconcileMigration(ctx, migration)
		span.End(err)
		if err != nil {
			status := migration.Status
			status.Phase = "Failed"
			status.Message = err.Error()
			a.Log.Error("migration reconcile failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", err)
			if a.Metrics != nil {
				a.Metrics.ObserveReconcileItemError("migration")
			}
			if statusErr := a.Kube.PatchMachineMigrationStatus(ctx, migration.Namespace(), migration.Metadata.Name, status); statusErr != nil {
				a.Log.Error("migration status patch failed", "namespace", migration.Namespace(), "migration", migration.Metadata.Name, "error", statusErr)
			}
		}
	}
	return nil
}

func (a *Agent) reconcileMachine(ctx context.Context, m model.Machine) error {
	if m.Metadata.DeletionTimestamp != nil {
		inFlight, err := a.migrationInFlightFor(ctx, m)
		if err != nil {
			return fmt.Errorf("check in-flight migrations before cleanup: %w", err)
		}
		if inFlight {
			return fmt.Errorf("refusing to delete the FluxVM runtime for %s/%s while a MachineMigration targeting it is still in flight; will retry once it reaches a terminal phase", m.Namespace(), m.Metadata.Name)
		}
		return a.cleanup(ctx, m)
	}
	if !model.HasFinalizer(m, model.Finalizer) {
		finalizers := append(append([]string{}, m.Metadata.Finalizers...), model.Finalizer)
		if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finalizers}}); err != nil {
			return err
		}
	}
	if m.DesiredPowerState() == "Stopped" {
		return a.ensureStopped(ctx, m)
	}
	if m.DesiredPowerState() == "Paused" {
		return a.ensurePaused(ctx, m)
	}
	if m.DesiredPowerState() == "Halted" {
		return a.ensureHalted(ctx, m)
	}
	// A template-based sandbox boots from FluxVM's own pre-baked template
	// spec, not spec.image/spec.volumes at all -- skip resolving a boot
	// disk entirely rather than demanding one that would just be ignored.
	// A plain (non-template) sandbox still needs one, same as any other
	// Machine. Likewise, spec.image.catalogName is a FluxVM catalog alias,
	// never a filesystem path kairon-node itself needs to resolve or fence
	// against --image-root.
	usingSandboxTemplate := m.Spec.Sandbox != nil && m.Spec.Sandbox.TemplateName != ""
	usingCatalogImage := m.Spec.Image.CatalogName != ""
	var volStatus csiVolumeStatus
	var dataDisks []fluxvm.DataDisk
	if !usingSandboxTemplate && !usingCatalogImage {
		if err := model.ValidateRootImage(m.Spec.Image); err != nil {
			return err
		}
		if m.Spec.Image.ImageRef != "" && m.Spec.Image.Source == nil {
			return fmt.Errorf("spec.image.imageRef %q is not resolved yet (kairon-controller pins it from the MachineImage)", m.Spec.Image.ImageRef)
		}
		// seedPath is what an empty spec.volumes[0] boot disk is seeded from.
		var seedPath string
		if m.Spec.Image.Source != nil {
			cachedPath, err := a.resolveImageSource(ctx, m)
			if err != nil {
				return err
			}
			if m.Spec.Image.Source.NeedsImport() {
				var imported importedImage
				if imported, err = a.resolveImportedImage(ctx, m, cachedPath); err != nil {
					return err
				}
				cachedPath, dataDisks = imported.Image, imported.dataDisks()
			}
			m.Spec.Image.Path, seedPath = cachedPath, cachedPath
		} else if m.Spec.Image.Blank && len(m.Spec.Volumes) == 0 {
			base, err := a.blankBase()
			if err != nil {
				return err
			}
			m.Spec.Image.Path = base
		}
		bootDisk, rbdBoot, err := a.resolveAtlasRBDBoot(m)
		if err != nil {
			return err
		}
		var vs csiVolumeStatus
		if rbdBoot {
			m.Spec.Image.Storage = fluxvm.StorageCephRBDInPlace
		} else if bootDisk, vs, err = a.resolveBootDiskPath(ctx, m); err != nil {
			return err
		}
		if bootDisk == "" {
			return fmt.Errorf("spec.image.path or spec.volumes[0] is required")
		}
		if !rbdBoot && len(m.Spec.Volumes) > 0 && (m.Spec.Image.Source != nil || m.Spec.Image.Blank) {
			var sizeGiB uint64
			if m.Spec.Image.DiskSize != "" {
				sizeGiB, _ = model.ParseDiskSizeGiB(m.Spec.Image.DiskSize)
			}
			if err := populateBootVolume(bootDisk, seedPath, sizeGiB); err != nil {
				return err
			}
		}
		if len(m.Spec.Volumes) == 0 && m.Spec.Image.Source == nil && !m.Spec.Image.Blank {
			// Only fence plain spec.image.path against ImageRoot -- a
			// PVC-resolved path already went through a stronger gate (the PVC
			// had to exist and be Bound, not just be a string any Machine
			// author could type in), and a Source-resolved path is
			// kairon-node's own cache directory, not something a Machine
			// author supplied, so the same node-local directory allowlist
			// doesn't apply to either.
			if err := a.validateImagePath(m); err != nil {
				return err
			}
		}
		m.Spec.Image.Path = bootDisk
		volStatus = vs
	}

	if !usingSandboxTemplate {
		m.Spec.Cdroms = append([]model.MachineCdrom(nil), m.Spec.Cdroms...)
		if err := a.resolveCdroms(ctx, &m); err != nil {
			return err
		}
	}

	var sharedFolders []fluxvm.SharedFolder
	if !usingSandboxTemplate {
		var err error
		sharedFolders, err = a.resolveDataVolumes(ctx, m)
		if err != nil {
			return err
		}
	}

	rec, err := a.current(ctx, m)
	if err != nil {
		return err
	}
	if rec == nil && model.AnnotationTrue(m.Metadata, model.AnnotationAdoptOnly) {
		status := m.Status
		status.Phase = "Blocked"
		status.NodeName = a.NodeName
		status.Message = "adopt-only cutover guard: incoming FluxVM runtime was not found; refusing to create a duplicate VM"
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "IncomingRuntimeMissing", Message: status.Message,
		})
		return a.patchMachineStatusIfChanged(ctx, m, status)
	}
	freshlyCreated := rec == nil
	restarted := false
	switch {
	case freshlyCreated && m.Metadata.Annotations[model.AnnotationForkFrom] != "":
		rec, err = a.forkRuntime(ctx, m)
		if err != nil {
			return err
		}
		a.Log.Info("forked runtime", "machine", m.Metadata.Name, "parent", m.Metadata.Annotations[model.AnnotationForkFrom], "runtimeID", rec.ID())
	case freshlyCreated && m.Spec.Sandbox != nil:
		boot := withBootResources(m)
		rec, err = a.Flux.CreateSandboxForMachine(ctx, boot)
		if err != nil {
			return err
		}
		a.Log.Info("created sandbox runtime", "machine", m.Metadata.Name, "runtimeID", rec.ID())
	case freshlyCreated:
		if err := model.ValidateCiliumAttach(m.Spec.Network); err != nil {
			return err
		}
		if m.Spec.Network.CiliumAttach {
			podUID := m.Spec.Network.PodUID
			if podUID == "" && m.Status.Network != nil && m.Status.Network.Cilium != nil {
				podUID = m.Status.Network.Cilium.ExternalWorkloadUID
			}
			if podUID == "" {
				return fmt.Errorf("ciliumAttach: awaiting CiliumExternalWorkload UID (controller has not projected podUID yet)")
			}
			m.Spec.Network.PodUID = podUID
		}
		vfioDevices, err := a.resolveVFIODevices(ctx, m)
		if err != nil {
			return err
		}
		boot := withBootResources(m)
		rec, err = a.Flux.CreateWithVFIO(ctx, boot, a.DefaultBackend, vfioDevices, sharedFolders, dataDisks)
		if err != nil {
			return err
		}
		a.Log.Info("created runtime", "machine", m.Metadata.Name, "runtimeID", rec.ID(), "vfioDevices", vfioDevices)
	case strings.EqualFold(rec.Status, "Paused") && m.DesiredPowerState() == "Running":
		// The only path back from Paused: ensurePaused above only ever
		// pauses, it's this branch (reached once spec.powerState is
		// edited back to Running) that resumes.
		rec, err = a.Flux.Resume(ctx, rec.ID())
		if err != nil {
			return err
		}
	case normalizePhase(rec.Status) == "Stopped" && m.DesiredPowerState() != "Stopped":
		// Reached only with DesiredPowerState() == "Running" -- Stopped,
		// Paused, and Halted all early-return above, before this chain.
		// Two distinct real callers land here:
		//  1. The normal Halted -> Running resume path: ensureHalted left
		//     the runtime FluxVM-stopped with its record kept, and the
		//     Machine spec was edited back to powerState: Running.
		//  2. A self-healing safety net: fluxvm.Client.RestoreSnapshot's
		//     own internal stop-then-start-from-snapshot orchestration
		//     also leaves a runtime FluxVM-stopped-with-record-kept if it
		//     crashes between the two steps -- recovering here restarts
		//     from the VM's last-known-good disk state, not the snapshot
		//     itself (RestoreSnapshot isn't retried automatically). A
		//     real external VMM crash, if FluxVM ever reports that the
		//     same way, would end up here too.
		// Both cases call the exact same FluxVM route (plain Start,
		// reusing FluxVM's own kept config) -- there is no way, or need,
		// to distinguish which of the three actually happened.
		rec, err = a.Flux.Start(ctx, rec.ID())
		if err != nil {
			return err
		}
		restarted = true
	}
	if !freshlyCreated && !usingSandboxTemplate {
		if rec, err = a.ejectRemovedCdroms(ctx, m, rec); err != nil {
			return err
		}
	}
	// Prune whatever CSI volume this Machine had staged/published *before*
	// this tick -- using m.Status, not the not-yet-committed status below
	// -- before it's overwritten by volStatus and lost. Fails closed: an
	// error here returns before status.Volume* is ever set to volStatus,
	// so the previous (still-accurate, still-mounted) volume keeps being
	// reported and this is retried next tick rather than silently
	// orphaning a live CSI attachment. See pruneStaleCSIVolume's own doc
	// comment.
	if err := a.pruneStaleCSIVolume(ctx, m, volStatus); err != nil {
		return err
	}
	status := m.Status
	status.Phase = normalizePhase(rec.Status)
	status.NodeName = a.NodeName
	status.RuntimeID = rec.ID()
	status.GuestIP = rec.GuestIP
	status.Message = ""
	status.VolumeStagingPath = volStatus.StagingPath
	status.VolumePublishPath = volStatus.PublishPath
	status.VolumeHandle = volStatus.VolumeID
	status.VolumeDriver = volStatus.Driver
	var (
		appliedVCPUs     uint32
		appliedMemoryMiB uint64
		hotplugErr       error
	)
	if restarted {
		appliedVCPUs, appliedMemoryMiB, hotplugErr = a.reconcileHotplugAfterRestart(ctx, m, rec)
	} else {
		appliedVCPUs, appliedMemoryMiB, hotplugErr = a.reconcileHotplug(ctx, m, rec, freshlyCreated)
	}
	status.AppliedVCPUs = appliedVCPUs
	status.AppliedMemoryMiB = appliedMemoryMiB
	if hotplugErr != nil {
		a.Log.Error("hotplug reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", hotplugErr)
		status.Message = hotplugErr.Error()
	}
	attachedDisks, diskVolumes, attachedIfaces, devErr := a.reconcileDevices(ctx, m, rec)
	status.AttachedDisks = attachedDisks
	status.DiskVolumes = diskVolumes
	status.AttachedInterfaces = attachedIfaces
	if devErr != nil {
		a.Log.Error("device hotplug reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", devErr)
		status.Message = devErr.Error()
	}
	appliedLimits, limitsErr := a.reconcileResourceLimits(ctx, m, rec)
	status.AppliedResourceLimits = appliedLimits
	if limitsErr != nil {
		a.Log.Error("resource limits reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", limitsErr)
		status.Message = limitsErr.Error()
	}
	// Best-effort, purely observational -- a stats read failing (e.g. a
	// FluxVM predating this endpoint) never blocks the rest of
	// reconcile; live usage is published to Prometheus when available.
	// ResourceUsage is volatile: it does not by itself trigger a status
	// patch (see patchMachineStatusIfChanged), so kaironctl top reflects
	// the last meaningful status write rather than every 3s tick.
	if usage, err := a.Flux.GetStats(ctx, rec.ID()); err == nil {
		status.ResourceUsage = &model.ResourceUsage{
			CPUPercent: usage.CPUUsagePercent, MemoryBytes: usage.MemoryUsageBytes,
			DiskReadBytes: usage.DiskReadBytes, DiskWriteBytes: usage.DiskWriteBytes,
		}
		if a.Metrics != nil {
			a.Metrics.ObserveMachineUsageFreshness(m.Namespace(), m.Metadata.Name)
		}
	}
	if err := a.projectNetworkStatus(ctx, m, rec, &status); err != nil {
		return err
	}
	a.reconcileGuestQuiesce(ctx, m, rec)
	appliedServiceFabric, err := a.reconcileServiceFabric(ctx, m, status.GuestIP)
	if err != nil {
		return err
	}
	status.AppliedServiceFabricMemberships = appliedServiceFabric
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Ready", Status: readyStatus(status.Phase), Reason: "FluxVMReconciled",
	})
	return a.patchMachineStatusIfChanged(ctx, m, status)
}

func (a *Agent) validateImagePath(m model.Machine) error {
	if a.ImageRoot == "" {
		return nil
	}
	root, err := filepath.Abs(a.ImageRoot)
	if err != nil {
		return fmt.Errorf("resolve image root: %w", err)
	}
	img, err := filepath.Abs(m.Spec.Image.Path)
	if err != nil {
		return fmt.Errorf("resolve image path: %w", err)
	}
	rel, err := filepath.Rel(root, img)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("image path %q is outside allowed root %q", m.Spec.Image.Path, a.ImageRoot)
	}
	return nil
}

func (a *Agent) current(ctx context.Context, m model.Machine) (*fluxvm.Record, error) {
	if m.Status.RuntimeID != "" {
		if rec, err := a.Flux.Get(ctx, m.Status.RuntimeID); err == nil {
			return rec, nil
		}
	}
	return a.Flux.LookupByName(ctx, m.RuntimeName())
}

func (a *Agent) ensureStopped(ctx context.Context, m model.Machine) error {
	rec, err := a.current(ctx, m)
	if err != nil {
		return err
	}
	if rec != nil && rec.ID() != "" {
		if err := a.Flux.Delete(ctx, rec.ID()); err != nil {
			return err
		}
	}
	// Deregister before status.guestIP is cleared below -- once it's gone
	// from status there's nothing left to tell FluxVM's Service Fabric to
	// remove. Fails closed: a real error here returns before the status
	// patch below runs, so this Machine keeps reporting its old Running
	// status (not falsely Stopped) and gets retried next tick rather than
	// silently leaving a now-dead backend registered. See
	// deregisterServiceFabric's own doc comment.
	if err := a.deregisterServiceFabric(ctx, m); err != nil {
		return fmt.Errorf("deregister service fabric membership: %w", err)
	}
	status := m.Status
	status.Phase = "Stopped"
	status.NodeName = a.NodeName
	status.RuntimeID = ""
	status.GuestIP = ""
	status.Network = nil
	status.Message = ""
	status.AppliedServiceFabricMemberships = nil
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Ready", Status: "False", Reason: "PoweredOff",
	})
	return a.patchMachineStatusIfChanged(ctx, m, status)
}

// ensurePaused suspends an already-running Machine's guest CPUs via
// FluxVM's real QMP pause (or backend equivalent -- backend-agnostic,
// unlike hotplug) -- RAM and device state stay fully resident, unlike
// ensureStopped's full teardown. Deliberately does not create a runtime
// that doesn't exist yet: pausing is only meaningful for a Machine
// that's already been realized against FluxVM at least once (the same
// "pause an existing VMI" semantics KubeVirt's own virtctl pause has,
// not "create one paused from the start").
func (a *Agent) ensurePaused(ctx context.Context, m model.Machine) error {
	rec, err := a.current(ctx, m)
	if err != nil {
		return err
	}
	status := m.Status
	if rec == nil {
		status.Phase = "Pending"
		status.NodeName = a.NodeName
		status.Message = "cannot pause: no existing runtime -- set powerState to Running first, then Paused"
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "NoRuntime", Message: status.Message,
		})
		return a.patchMachineStatusIfChanged(ctx, m, status)
	}
	if !strings.EqualFold(rec.Status, "Paused") {
		rec, err = a.Flux.Pause(ctx, rec.ID())
		if err != nil {
			return err
		}
	}
	status.Phase = normalizePhase(rec.Status)
	status.NodeName = a.NodeName
	status.RuntimeID = rec.ID()
	status.GuestIP = rec.GuestIP
	status.Message = ""
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Ready", Status: "False", Reason: "Paused",
	})
	return a.patchMachineStatusIfChanged(ctx, m, status)
}

// ensureHalted powers a Machine off via FluxVM's real Stop (the VMM
// process terminates, guest RAM is lost) while FluxVM keeps its own VM
// record and disk intact -- a genuinely different operation from
// ensureStopped's full teardown (FluxVM DELETE, status.runtimeID
// cleared, the runtime rebuilt from spec on the next Running). Frees the
// node's resident RAM/CPU the same way Stopped does (unlike Paused,
// which keeps the VMM process alive and still counts against scheduler
// capacity -- see countAssigned in internal/controller/controller.go),
// but keeps FluxVM's own last-applied VM config so the reconcile
// self-healing branch above (rec.Status normalizes to Stopped, desired
// is Running) can bring it back via a plain Start rather than a full
// Kairon-side recreate. Only a single-hop resume back to Running is
// handled directly -- the same convention ensurePaused's own doc comment
// establishes for Paused; Halted -> Paused isn't special-cased here, go
// through Running first.
func (a *Agent) ensureHalted(ctx context.Context, m model.Machine) error {
	rec, err := a.current(ctx, m)
	if err != nil {
		return err
	}
	status := m.Status
	if rec == nil {
		status.Phase = "Pending"
		status.NodeName = a.NodeName
		status.Message = "cannot halt: no existing runtime -- set powerState to Running first, then Halted"
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "NoRuntime", Message: status.Message,
		})
		return a.patchMachineStatusIfChanged(ctx, m, status)
	}
	if normalizePhase(rec.Status) != "Stopped" {
		rec, err = a.Flux.Stop(ctx, rec.ID())
		if err != nil {
			return err
		}
	}
	// Same reasoning and fail-closed posture as ensureStopped's own call:
	// Halted's guest is powered off just the same (only the runtime
	// record is kept, for a plain Start back to Running), so it's just as
	// stale a Service Fabric backend once status.guestIP below is
	// cleared. See deregisterServiceFabric's own doc comment.
	if err := a.deregisterServiceFabric(ctx, m); err != nil {
		return fmt.Errorf("deregister service fabric membership: %w", err)
	}
	// Deliberately not normalizePhase(rec.Status) -- FluxVM's own Stop
	// reports the same raw status ("stopped"/"exited") ensureStopped's
	// full-teardown path does, but Halted keeps the runtime record (and
	// is resumable via a plain Start, not a full recreate), so it needs
	// its own distinct Machine-level phase rather than reading as
	// "Stopped" to callers/the dashboard/scheduler capacity accounting.
	status.Phase = "Halted"
	status.NodeName = a.NodeName
	status.RuntimeID = rec.ID()
	status.GuestIP = ""
	status.Network = nil
	status.Message = ""
	status.AppliedServiceFabricMemberships = nil
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Ready", Status: "False", Reason: "Halted",
	})
	return a.patchMachineStatusIfChanged(ctx, m, status)
}

// isNonTerminalMigrationPhase deliberately duplicates (rather than
// imports) internal/controller's own isTerminalMigrationPhase and
// internal/uiapi's isNonTerminalMigrationPhase -- matching this
// codebase's own established precedent (see disruption.go's own comment)
// that each consumer keeps its narrower definition of "terminal" rather
// than share one across packages for a single boolean.
func isNonTerminalMigrationPhase(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Blocked", "":
		return false
	}
	return true
}

// migrationInFlightFor reports whether a live QEMU-level migration
// transfer may currently be streaming this Machine's runtime state --
// i.e. whether some MachineMigration in m's namespace names m as its
// Spec.MachineName and hasn't yet reached a terminal phase. A QMP
// "migrate" command is asynchronous on the hypervisor side: once
// reconcileMigration starts one, it keeps running whether or not this
// reconcile loop is watching it, so cleanup deleting the source runtime
// out from under an in-flight transfer would race a live migration --
// exactly the kind of ambiguous outcome NeedsRecovery exists to name
// explicitly, not something to risk by proceeding regardless.
func (a *Agent) migrationInFlightFor(ctx context.Context, m model.Machine) (bool, error) {
	migrations, err := a.Kube.ListMachineMigrationsNamespace(ctx, m.Namespace())
	if err != nil {
		return false, err
	}
	for _, mig := range migrations {
		if mig.Spec.MachineName == m.Metadata.Name && isNonTerminalMigrationPhase(mig.Status.Phase) {
			return true, nil
		}
	}
	return false, nil
}

func (a *Agent) cleanup(ctx context.Context, m model.Machine) error {
	if m.Status.RuntimeID != "" {
		if err := a.Flux.Delete(ctx, m.Status.RuntimeID); err != nil {
			return err
		}
	}
	// Same "don't finish deleting until this succeeds" posture as the
	// FluxVM runtime delete above -- a failed unpublish/unstage leaves
	// the finalizer in place (this Machine stays around, reconciled
	// again next tick) rather than silently leaking a mounted iSCSI
	// session. A no-op for the overwhelmingly common case: plain
	// spec.image.path, or a hostPath/local-backed volume.
	if err := a.teardownCSIVolume(ctx, m); err != nil {
		return fmt.Errorf("tear down CSI volume: %w", err)
	}
	if err := a.teardownDiskVolumes(ctx, m); err != nil {
		return fmt.Errorf("tear down CSI disk volumes: %w", err)
	}
	// Same fail-closed posture as the FluxVM runtime delete and CSI
	// teardown above: a real deregistration error leaves the finalizer in
	// place rather than letting this Machine vanish from Kubernetes while
	// its guest IP stays a live backend behind a Service Fabric VIP. See
	// deregisterServiceFabric's own doc comment.
	if err := a.deregisterServiceFabric(ctx, m); err != nil {
		return fmt.Errorf("deregister service fabric membership: %w", err)
	}
	var finals []string
	for _, f := range m.Metadata.Finalizers {
		if f != model.Finalizer {
			finals = append(finals, f)
		}
	}
	return a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}})
}

func (a *Agent) resolveVFIODevices(ctx context.Context, m model.Machine) ([]string, error) {
	if len(m.Spec.DeviceClaims) == 0 {
		return nil, nil
	}
	if len(a.VFIOAllowlist) == 0 {
		return nil, fmt.Errorf("machine requests DRA devices but KAIRON_VFIO_ALLOWLIST is empty; refusing unapproved VFIO passthrough")
	}
	seen := map[string]struct{}{}
	for _, ref := range m.Spec.DeviceClaims {
		if strings.TrimSpace(ref.Name) == "" {
			return nil, fmt.Errorf("deviceClaims contains an empty ResourceClaim name")
		}
		claim, err := a.Kube.GetResourceClaim(ctx, m.Namespace(), ref.Name)
		if err != nil {
			return nil, fmt.Errorf("get ResourceClaim %s: %w", ref.Name, err)
		}
		if claim.Status.Allocation == nil {
			return nil, fmt.Errorf("ResourceClaim %s has no DRA allocation yet", ref.Name)
		}
		var candidates []string
		if claim.Metadata.Annotations != nil {
			for _, raw := range strings.Split(claim.Metadata.Annotations[model.AnnotationVFIOBDF], ",") {
				if v := strings.TrimSpace(raw); v != "" {
					candidates = append(candidates, v)
				}
			}
		}
		if claim.Status.Allocation != nil {
			for _, r := range claim.Status.Allocation.Devices.Results {
				if pciBDFPattern.MatchString(strings.TrimSpace(r.Device)) {
					candidates = append(candidates, r.Device)
				}
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("ResourceClaim %s is allocated but exposes no PCI BDF; set %s or use a DRA driver whose device result is a BDF", ref.Name, model.AnnotationVFIOBDF)
		}
		for _, raw := range candidates {
			bdf, err := NormalizeBDF(raw)
			if err != nil {
				return nil, fmt.Errorf("ResourceClaim %s: %w", ref.Name, err)
			}
			if _, ok := a.VFIOAllowlist[bdf]; !ok {
				return nil, fmt.Errorf("VFIO device %s from ResourceClaim %s is not in this node's allowlist", bdf, ref.Name)
			}
			seen[bdf] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for bdf := range seen {
		out = append(out, bdf)
	}
	sort.Strings(out)
	return out, nil
}

func ParseVFIOAllowlist(raw string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		bdf, err := NormalizeBDF(item)
		if err != nil {
			return nil, err
		}
		out[bdf] = struct{}{}
	}
	return out, nil
}

// ParseThirdPartyCSIDrivers parses "-third-party-csi-drivers"/
// $KAIRON_THIRD_PARTY_CSI_DRIVERS -- a comma-separated
// "driverName=/socket/path" list, mirroring ParseVFIOAllowlist's own
// simple format. See Agent.ThirdPartyCSIDrivers's own doc comment for
// what this actually authorizes.
func ParseThirdPartyCSIDrivers(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		driver, socket, ok := strings.Cut(item, "=")
		driver, socket = strings.TrimSpace(driver), strings.TrimSpace(socket)
		if !ok || driver == "" || socket == "" {
			return nil, fmt.Errorf("invalid third-party CSI driver entry %q -- expected driverName=/socket/path", item)
		}
		out[driver] = socket
	}
	return out, nil
}

func NormalizeBDF(raw string) (string, error) {
	bdf := strings.ToLower(strings.TrimSpace(raw))
	if !pciBDFPattern.MatchString(bdf) {
		return "", fmt.Errorf("invalid PCI BDF %q", raw)
	}
	if strings.Count(bdf, ":") == 1 {
		bdf = "0000:" + bdf
	}
	return bdf, nil
}

func (a *Agent) reconcileMigration(ctx context.Context, item model.MachineMigration) error {
	phase := item.Status.Phase
	if phase == "NeedsRecovery" {
		return a.reconcileNeedsRecovery(ctx, item)
	}
	if phase != "Starting" && phase != "Running" {
		return nil
	}
	if item.Status.EffectiveStrategy != "live" {
		return nil
	}
	if item.Spec.Cancel {
		return a.cancelLiveMigration(ctx, item)
	}
	machine, err := a.Kube.GetMachine(ctx, item.Namespace(), item.Spec.MachineName)
	if err != nil {
		return err
	}
	if machine.Spec.NodeName != a.NodeName {
		return fmt.Errorf("source Machine is assigned to %s, not %s", machine.Spec.NodeName, a.NodeName)
	}
	backend := machine.Spec.Runtime.Backend
	if backend != "" && backend != "auto" && backend != "qemu" {
		return fmt.Errorf("live migration currently requires qemu backend, Machine requests %s", backend)
	}
	if a.MigrationPeer == nil || a.SourceMigrator == nil {
		return a.blockLiveMigration(ctx, item, "secure migration control plane or source adapter is not configured; source runtime was left untouched")
	}

	rec, err := a.current(ctx, machine)
	if err != nil {
		return err
	}
	if rec == nil || rec.ID() == "" {
		return fmt.Errorf("source FluxVM runtime not found")
	}
	session := migration.Session{
		ID:         migrationSessionID(item),
		Namespace:  item.Namespace(),
		Machine:    item.Spec.MachineName,
		SourceNode: item.Status.SourceNode,
		TargetNode: item.Status.TargetNode,
		RuntimeID:  rec.ID(),
		DiskPath:   rec.Disk,
		MAC:        rec.Request.Network.MAC,
		VCPUs:      rec.Request.VCPUs,
		MemoryMiB:  rec.Request.MemoryMiB,

		MigrationNetwork: item.Spec.MigrationNetwork,
	}
	var targetURL string
	if a.MigrationPeerURL != nil {
		targetURL, err = a.MigrationPeerURL(ctx, item.Status.TargetNode)
	} else {
		targetURL, err = a.targetControlURL(ctx, item.Status.TargetNode)
	}
	if err != nil {
		return err
	}

	status := item.Status
	if phase == "Starting" {
		if err := a.Flux.NetworkMigrationQuiesce(ctx, rec.ID()); err != nil {
			a.Log.Warn("network migration quiesce failed; continuing", "runtimeID", rec.ID(), "error", err)
		} else if snap, err := a.Flux.NetworkMigrationExport(ctx, rec.ID()); err != nil {
			a.Log.Warn("network migration export failed; continuing without snapshot", "runtimeID", rec.ID(), "error", err)
		} else {
			session.NetworkSnapshot = snap
		}
		mode := item.Spec.Mode
		if mode == "" {
			mode = "pre-copy"
		}
		if mode != "pre-copy" && mode != "post-copy" {
			a.resumeSourceNetworkQuiesce(ctx, rec.ID())
			return fmt.Errorf("unsupported migration mode %q", mode)
		}
		caps, tuner := a.sourceCapabilities(ctx)
		plan, err := planLiveStream(item, machine, rec, caps, mode)
		if err != nil {
			a.resumeSourceNetworkQuiesce(ctx, rec.ID())
			return a.blockLiveMigration(ctx, item, err.Error()+"; source runtime was left untouched")
		}
		session.Stream = &plan.stream
		session.ConntrackSnapshot = a.exportConntrackSnapshot(ctx, machine, rec.ID())
		if raw, err := a.Flux.GetRaw(ctx, rec.ID()); err != nil {
			a.Log.Warn("reading source FluxVM record for the receiver failed; continuing without it", "runtimeID", rec.ID(), "error", err)
		} else {
			session.SourceRecord = raw
		}
		prepared, err := a.MigrationPeer.Prepare(ctx, targetURL, session)
		if err != nil {
			return fmt.Errorf("prepare target %s: %w", item.Status.TargetNode, err)
		}
		status.SessionID = prepared.SessionID
		status.RuntimeID = rec.ID()
		status.Backend = prepared.Backend
		status.TransferPhase = prepared.Phase
		status.DataPlaneEncrypted = prepared.DataPlaneEncrypted
		if !prepared.DataPlaneEncrypted {
			a.Log.Warn("live migration data-plane is not encrypted", "migration", item.Metadata.Name, "namespace", item.Namespace())
		}
		if !prepared.TransferSupported {
			message := prepared.Reason
			if message == "" {
				message = "target node has no live migration backend"
			}
			a.resumeSourceNetworkQuiesce(ctx, rec.ID())
			return a.blockLiveMigrationWithStatus(ctx, item, status, message+"; source runtime was left untouched")
		}
		opts, notes, err := sourceStartOptions(item, mode, plan, prepared.Stream, caps)
		if err != nil {
			_ = a.MigrationPeer.Abort(ctx, targetURL, session.ID)
			a.resumeSourceNetworkQuiesce(ctx, rec.ID())
			return a.blockLiveMigrationWithStatus(ctx, item, status, err.Error()+"; prepared target was aborted and source runtime was left untouched")
		}
		transfer, err := a.SourceMigrator.Start(ctx, migration.SourceRequest{
			Session:   session,
			RuntimeID: rec.ID(),
			Endpoint:  prepared.Endpoint,
			Options:   opts,
		})
		if err != nil {
			_ = a.MigrationPeer.Abort(ctx, targetURL, session.ID)
			a.resumeSourceNetworkQuiesce(ctx, rec.ID())
			if errors.Is(err, migration.ErrUnsupported) {
				return a.blockLiveMigrationWithStatus(ctx, item, status, err.Error()+"; prepared target was aborted and source runtime was left untouched")
			}
			status.Phase = "Failed"
			status.TransferPhase = "start-failed"
			status.Message = "start source transfer failed; prepared target was aborted: " + err.Error()
			return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
		}
		status.TransferID = transfer.TransferID
		status.Adaptive = adaptiveStart(item, opts, caps, tuner, notes, time.Now())
		return a.projectTransfer(ctx, item, status, session, targetURL, transfer)
	}

	if status.SessionID == "" || status.TransferID == "" {
		return fmt.Errorf("running migration is missing sessionID or transferID")
	}
	transfer, err := a.SourceMigrator.Status(ctx, session, status.TransferID)
	if err != nil {
		return fmt.Errorf("poll source transfer: %w", err)
	}
	// Best-effort: renews the destination's Prepared-session lease once per
	// reconcile tick so its own Server.ReapStaleSessions (opt-in via a
	// non-zero HeartbeatTTL) doesn't mistake an actively-transferring
	// migration for an abandoned one. Never fails the reconcile over a
	// missed heartbeat -- the same posture as resumeSourceNetworkQuiesce.
	if err := a.MigrationPeer.Heartbeat(ctx, targetURL, session.ID); err != nil {
		a.Log.Warn("migration heartbeat to target failed; continuing", "migration", item.Metadata.Name, "namespace", item.Namespace(), "target", item.Status.TargetNode, "error", err)
	}
	return a.projectTransfer(ctx, item, status, session, targetURL, transfer)
}

func (a *Agent) projectTransfer(ctx context.Context, item model.MachineMigration, status model.MachineMigrationStatus, session migration.Session, targetURL string, transfer migration.TransferStatus) error {
	phase := strings.ToLower(strings.TrimSpace(transfer.Phase))
	status.TransferPhase = phase
	status.RAMTransferred = transfer.RAMTransferred
	status.RAMRemaining = transfer.RAMRemaining
	status.RAMTotal = transfer.RAMTotal
	status.TotalTimeMs = transfer.TotalTimeMs
	status.DowntimeMs = transfer.DowntimeMs
	if transfer.TransferID != "" {
		status.TransferID = transfer.TransferID
	}
	if p := transferProgress(transfer, item.Spec.Adaptive != nil && item.Spec.Adaptive.Compression != nil && item.Spec.Adaptive.Compression.Algorithm != model.MigrationCompressionNone, time.Now()); p != nil {
		status.Progress = p
	}
	if transfer.PostCopyActive {
		status.Adaptive = postCopyLatched(status.Adaptive, time.Now())
	}
	switch phase {
	case "completed", "transferred":
		if err := a.MigrationPeer.Commit(ctx, targetURL, session.ID); err != nil {
			status.Phase = "NeedsRecovery"
			status.Message = "source transfer completed but target commit failed; automatic cutover is stopped to avoid split brain: " + err.Error()
			return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
		}
		a.finishSourceRuntime(ctx, session, status.TransferID)
		status.Phase = "Cutover"
		status.Message = "source transfer completed and target committed; waiting for guarded target adoption"
	case "failed", "cancelled", "canceled", "aborted":
		if postCopyStarted(status) {
			// After the switch the guest's newest memory lives on the
			// destination and the rest on the source. Resuming the source
			// would run a stale guest; aborting the destination would lose
			// the live one.
			status.Phase = "NeedsRecovery"
			status.Message = "source transfer " + phase + " after post-copy started; the source was not resumed and the destination was not aborted -- inspect both nodes before choosing a recovery action"
			if transfer.Message != "" {
				status.Message += ": " + transfer.Message
			}
			break
		}
		_ = a.MigrationPeer.Abort(ctx, targetURL, session.ID)
		a.resumeSourceNetworkQuiesce(ctx, session.RuntimeID)
		status.Phase = "Failed"
		status.Message = transfer.Message
		if status.Message == "" {
			status.Message = "source transfer " + phase + "; prepared target aborted"
		}
	default:
		status.Phase = "Running"
		status.Message = "live migration transfer in progress"
		if abort, reason := a.applyAdaptive(ctx, item, &status, session, transfer); abort {
			if err := a.SourceMigrator.Abort(ctx, session, status.TransferID); err != nil {
				a.Log.Warn("adaptive abort: source transfer abort failed", "migration", item.Metadata.Name, "namespace", item.Namespace(), "error", err)
			}
			_ = a.MigrationPeer.Abort(ctx, targetURL, session.ID)
			a.resumeSourceNetworkQuiesce(ctx, session.RuntimeID)
			status.Phase = "Failed"
			status.TransferPhase = "aborted"
			status.Message = "adaptive engine aborted the transfer: " + reason + "; prepared target aborted and source runtime left running"
		} else if status.Adaptive != nil && status.Adaptive.Stage != "" {
			status.Message = "live migration transfer in progress (" + status.Adaptive.Stage + ")"
		}
	}
	return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
}

// reconcileNeedsRecovery never auto-resolves a NeedsRecovery migration --
// every tick it only refreshes a live diagnosis of what's actually known
// (status.Recovery's diagnosis fields), and acts *only* when the operator
// has set spec.recovery with an AcknowledgedDiagnosis that matches the
// requested Action and a non-empty Reason. See docs/architecture.md and
// SECURITY.md: an ambiguous commit must never auto-resolve.
func (a *Agent) reconcileNeedsRecovery(ctx context.Context, item model.MachineMigration) error {
	status := item.Status
	if a.MigrationPeer == nil {
		return fmt.Errorf("secure migration control plane is not configured; cannot diagnose NeedsRecovery")
	}
	var targetURL string
	var err error
	if a.MigrationPeerURL != nil {
		targetURL, err = a.MigrationPeerURL(ctx, item.Status.TargetNode)
	} else {
		targetURL, err = a.targetControlURL(ctx, item.Status.TargetNode)
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	recovery := status.Recovery
	if recovery == nil {
		recovery = &model.MachineMigrationRecoveryStatus{}
	}
	recovery.DiagnosedAt = &now
	if status.RuntimeID != "" {
		if rec, err := a.Flux.Get(ctx, status.RuntimeID); err != nil {
			a.Log.Warn("NeedsRecovery: source runtime lookup failed", "runtimeID", status.RuntimeID, "error", err)
			recovery.SourceRuntimeStatus = ""
		} else {
			recovery.SourceRuntimeStatus = rec.Status
		}
	}
	if status.SessionID != "" {
		diag, err := a.MigrationPeer.Diagnose(ctx, targetURL, status.SessionID)
		if err != nil {
			a.Log.Warn("NeedsRecovery: destination diagnosis failed", "sessionID", status.SessionID, "error", err)
		} else {
			recovery.DestinationSessionPhase = diag.SessionPhase
			recovery.DestinationRuntimeFound = diag.DestinationRuntimeFound
			recovery.DestinationRuntimeStatus = diag.DestinationRuntimeStatus
		}
	}
	status.Recovery = recovery

	action := item.Spec.Recovery
	if action == nil {
		// Parked, but the operator can now see live diagnosis via `kubectl
		// get machinemigration -o yaml` without triggering anything.
		return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
	}
	if strings.TrimSpace(action.Reason) == "" {
		status.Message = "spec.recovery.reason is required -- refusing to act without the operator's attested evidence"
		return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
	}
	diagnosisMatches := map[string]string{
		model.RecoveryActionConfirmDestinationCommitted:    model.RecoveryDiagnosisDestinationCommitted,
		model.RecoveryActionConfirmDestinationNotCommitted: model.RecoveryDiagnosisDestinationNotCommitted,
	}
	if want, ok := diagnosisMatches[action.Action]; ok && action.AcknowledgedDiagnosis != want {
		status.Message = fmt.Sprintf(
			"spec.recovery.acknowledgedDiagnosis (%s) does not match spec.recovery.action (%s); refusing to act -- update spec.recovery to match what you actually observed",
			action.AcknowledgedDiagnosis, action.Action,
		)
		return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
	} else if !ok && action.Action != model.RecoveryActionForceAbort {
		status.Message = fmt.Sprintf("unknown spec.recovery.action %q", action.Action)
		return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
	}

	appliedAt := time.Now().UTC()
	recovery.AppliedAction = action.Action
	recovery.AppliedReason = action.Reason
	recovery.AppliedAcknowledgedDiagnosis = action.AcknowledgedDiagnosis
	recovery.AppliedAt = &appliedAt

	switch action.Action {
	case model.RecoveryActionConfirmDestinationCommitted:
		// Idempotent: heals the case where the destination actually
		// committed but a prior network-resume step failed before the
		// session store could be updated to "Committed" (see
		// NetworkAwareDestination.Commit's own doc comment).
		if err := a.MigrationPeer.Commit(ctx, targetURL, status.SessionID); err != nil {
			a.Log.Warn("NeedsRecovery: ConfirmDestinationCommitted re-commit failed; proceeding anyway on operator's attestation", "sessionID", status.SessionID, "error", err)
		}
		if status.RuntimeID != "" {
			if err := a.Flux.Delete(ctx, status.RuntimeID); err != nil {
				a.Log.Warn("NeedsRecovery: best-effort source runtime delete failed", "runtimeID", status.RuntimeID, "error", err)
			}
		}
		status.Phase = "Cutover"
		status.Message = "recovery: operator confirmed destination committed; source runtime removed, handing off to normal cutover"
	case model.RecoveryActionConfirmDestinationNotCommitted:
		if err := a.MigrationPeer.Commit(ctx, targetURL, status.SessionID); err != nil {
			_ = a.MigrationPeer.Abort(ctx, targetURL, status.SessionID)
			status.Phase = "Failed"
			status.Message = "recovery: retried commit failed, confirming destination never committed: " + err.Error() + "; host-level inspection is required before reusing this Machine"
			break
		}
		if status.RuntimeID != "" {
			if err := a.Flux.Delete(ctx, status.RuntimeID); err != nil {
				a.Log.Warn("NeedsRecovery: best-effort source runtime delete failed", "runtimeID", status.RuntimeID, "error", err)
			}
		}
		status.Phase = "Cutover"
		status.Message = "recovery: retried commit succeeded; source runtime removed, handing off to normal cutover"
	case model.RecoveryActionForceAbort:
		// Deliberately never touches the source runtime here: ground truth
		// is unknown by definition, so destroying the only possibly-live
		// copy of the guest is out of bounds. A 409 from Abort is expected
		// and fine if the destination secretly did commit.
		if err := a.MigrationPeer.Abort(ctx, targetURL, status.SessionID); err != nil {
			a.Log.Warn("NeedsRecovery: ForceAbort peer abort failed", "sessionID", status.SessionID, "error", err)
		}
		status.Phase = "Failed"
		status.Message = "recovery: operator forced abort; source runtime was left untouched -- verify its state before reuse"
	}
	return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
}

// finishSourceRuntime releases the paused source runtime once the
// destination has committed, when the adapter supports it. Best effort:
// the guest already runs on the destination, so a failure only leaks the
// paused source process, which is logged for the operator.
func (a *Agent) finishSourceRuntime(ctx context.Context, session migration.Session, transferID string) {
	f, ok := a.SourceMigrator.(migration.Finisher)
	if !ok || transferID == "" {
		return
	}
	if err := f.Finish(ctx, session, transferID); err != nil {
		a.Log.Warn("releasing the paused source runtime failed; it keeps its memory until removed", "runtimeID", session.RuntimeID, "error", err)
	}
}

// resumeSourceNetworkQuiesce best-effort un-quiesces runtimeID's own network
// dataplane on the source node, undoing reconcileMigration's own
// NetworkMigrationQuiesce for every path where a live migration stops short
// of a destination commit -- blocked, an unsupported mode, a source
// transfer that never started, or a transfer that failed/was
// cancelled/aborted. Without this, the source Machine -- which stays the
// real, running VM on every one of these paths, unlike the destination --
// would have its network left quiesced indefinitely: nothing else in this
// codebase ever calls NetworkMigrationResume for the source runtime, only
// the destination side resumes its own (internal/migration/network.go's
// NetworkAwareDestination.Commit), which never runs when the migration
// doesn't reach that point. Best-effort and log-only on failure, matching
// NetworkMigrationQuiesce/Export's own established convention in
// reconcileMigration: failing to resume must never turn an already-bad
// migration outcome into a Go error that blocks this reconcile loop's other
// work. Deliberately not called from reconcileNeedsRecovery -- an ambiguous
// commit must never be touched further by anything but an operator's own
// attested decision, matching this project's own documented principle.
func (a *Agent) resumeSourceNetworkQuiesce(ctx context.Context, runtimeID string) {
	if runtimeID == "" {
		return
	}
	if err := a.Flux.NetworkMigrationResume(ctx, runtimeID); err != nil {
		a.Log.Warn("resuming source network migration quiesce failed", "runtimeID", runtimeID, "error", err)
	}
}

// cancelLiveMigration honors an operator's spec.cancel request against a
// live migration still in Starting or Running -- the one window where
// aborting is safe, since the destination has not yet committed. It reuses
// exactly the same source/destination Abort primitives an unrequested
// transfer failure already calls in projectTransfer, best-effort (a failed
// abort call is logged, never turned into a reconcile error that would
// leave spec.cancel stuck retrying forever): the source runtime is the
// only guaranteed-live copy of the guest regardless of whether the abort
// calls themselves succeed, so this always lands Cancelled and always
// leaves the source untouched. Never called once phase has reached
// Cutover or later -- reconcileMigration only routes here from the
// Starting/Running branch, and the top-level terminal-phase check in
// internal/controller/controller.go's reconcileMigration stops even this
// function from ever being reached for a migration that already moved on.
func (a *Agent) cancelLiveMigration(ctx context.Context, item model.MachineMigration) error {
	status := item.Status
	if postCopyStarted(status) {
		if strings.HasPrefix(status.Message, "cancel ignored") {
			return nil
		}
		status.Message = "cancel ignored: post-copy has started, so the source can no longer take the guest back; the migration continues"
		return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
	}
	sessionID := status.SessionID
	if sessionID == "" {
		sessionID = migrationSessionID(item)
	}
	session := migration.Session{ID: sessionID}

	if a.SourceMigrator != nil && status.TransferID != "" {
		if err := a.SourceMigrator.Abort(ctx, session, status.TransferID); err != nil {
			a.Log.Warn("cancel: source transfer abort failed", "migration", item.Metadata.Name, "namespace", item.Namespace(), "error", err)
		}
	}
	if a.MigrationPeer != nil && status.SessionID != "" {
		var targetURL string
		var err error
		if a.MigrationPeerURL != nil {
			targetURL, err = a.MigrationPeerURL(ctx, item.Status.TargetNode)
		} else {
			targetURL, err = a.targetControlURL(ctx, item.Status.TargetNode)
		}
		if err != nil {
			a.Log.Warn("cancel: resolving target control URL failed; destination session may be left dangling until its own heartbeat/TTL reaper cleans it up", "migration", item.Metadata.Name, "namespace", item.Namespace(), "error", err)
		} else if err := a.MigrationPeer.Abort(ctx, targetURL, status.SessionID); err != nil {
			a.Log.Warn("cancel: destination abort failed", "migration", item.Metadata.Name, "namespace", item.Namespace(), "error", err)
		}
	}
	a.resumeSourceNetworkQuiesce(ctx, status.RuntimeID)

	status.Phase = "Cancelled"
	status.Message = "operator requested cancel (spec.cancel); in-flight live migration aborted before target commit, source runtime was left untouched"
	return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
}

func (a *Agent) blockLiveMigration(ctx context.Context, item model.MachineMigration, message string) error {
	return a.blockLiveMigrationWithStatus(ctx, item, item.Status, message)
}

func (a *Agent) blockLiveMigrationWithStatus(ctx context.Context, item model.MachineMigration, status model.MachineMigrationStatus, message string) error {
	status.Phase = "Blocked"
	status.Message = message
	return a.Kube.PatchMachineMigrationStatus(ctx, item.Namespace(), item.Metadata.Name, status)
}

func migrationSessionID(item model.MachineMigration) string {
	seed := item.Metadata.UID
	if seed == "" {
		seed = item.Namespace() + "/" + item.Metadata.Name
	}
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("kmm-%x", sum[:16])
}

func (a *Agent) targetControlURL(ctx context.Context, targetNode string) (string, error) {
	if targetNode == "" {
		return "", fmt.Errorf("migration target node is empty")
	}
	nodes, err := a.Kube.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	var address string
	for _, node := range nodes {
		if node.Metadata.Name != targetNode {
			continue
		}
		for _, candidate := range node.Status.Addresses {
			if candidate.Type == "InternalIP" && strings.TrimSpace(candidate.Address) != "" {
				address = strings.TrimSpace(candidate.Address)
				break
			}
		}
		break
	}
	if address == "" {
		return "", fmt.Errorf("target node %q has no InternalIP for the mTLS migration control plane", targetNode)
	}
	port := a.MigrationPort
	if port == 0 {
		port = 9443
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid migration port %d", port)
	}
	return "https://" + net.JoinHostPort(address, fmt.Sprintf("%d", port)), nil
}

func normalizePhase(s string) string {
	s = strings.ToLower(s)
	switch s {
	case "running", "started", "ready":
		return "Running"
	case "paused":
		return "Paused"
	case "stopped", "exited":
		return "Stopped"
	case "creating", "starting", "pending":
		return "Starting"
	default:
		if s == "" {
			return "Running"
		}
		return "Unknown"
	}
}
func readyStatus(phase string) string {
	if phase == "Running" {
		return "True"
	}
	return "False"
}

// patchMachineStatusIfChanged sets observedGeneration, publishes live
// ResourceUsage to Prometheus when present, and skips the Kubernetes
// status PATCH when the meaningful status is unchanged (ResourceUsage is
// treated as volatile and does not alone force a write). When a patch is
// needed, the fresh ResourceUsage is included so the last meaningful
// write still carries a recent sample for kaironctl top.
func (a *Agent) patchMachineStatusIfChanged(ctx context.Context, m model.Machine, status model.MachineStatus) error {
	status.ObservedGeneration = m.Metadata.Generation
	if a.Metrics != nil {
		a.Metrics.ObserveMachineResourceUsage(m.Namespace(), m.Metadata.Name, status.ResourceUsage)
	}
	if model.MachineStatusEqualIgnoringVolatile(m.Status, status) {
		return nil
	}
	if err := a.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
		return err
	}
	// Best-effort Event so `kaironctl events` / `kubectl get events` show
	// lifecycle transitions; a failure here must never fail reconcile.
	if status.Phase != "" && status.Phase != m.Status.Phase {
		from := m.Status.Phase
		if from == "" {
			from = "None"
		}
		typ := "Normal"
		if status.Phase == "Failed" {
			typ = "Warning"
		}
		if err := a.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, "Phase"+status.Phase, "phase "+from+" -> "+status.Phase, typ); err != nil {
			a.log().Debug("record phase event failed", "machine", m.Metadata.Name, "error", err)
		}
	}
	return nil
}

func (a *Agent) Run(ctx context.Context, interval time.Duration) error {
	// Watch wakes reconcile promptly; interval is the safety resync only.
	wake := make(chan struct{}, 1)
	kick := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	kick() // initial sync

	go a.watchAssigned(ctx, kick)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := time.Now()
		err := a.Reconcile(ctx)
		if a.Metrics != nil {
			a.Metrics.ObserveReconcile(time.Since(start), err)
		}
		if err != nil {
			a.Log.Error("reconcile failed", "error", err)
		}
		if a.LivenessLeaseNamespace != "" {
			if err := nodeliveness.Renew(ctx, a.Kube, a.LivenessLeaseNamespace, a.NodeName, a.LivenessLeaseDuration); err != nil {
				a.Log.Warn("liveness lease renewal failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-t.C:
		}
	}
}

// watchAssigned runs label-scoped watches for Machines and migrations
// assigned to this node. Failures are logged and retried; the resync
// ticker remains the safety net.
func (a *Agent) watchAssigned(ctx context.Context, kick func()) {
	for {
		if ctx.Err() != nil {
			return
		}
		watchCtx, cancel := context.WithCancel(ctx)
		events := make(chan kube.WatchEvent, 32)
		errCh := make(chan error, 2)
		go func() {
			errCh <- a.Kube.WatchMachines(watchCtx, model.AssignedNodeLabelSelector(a.NodeName), "", events)
		}()
		go func() {
			errCh <- a.Kube.WatchMachineMigrations(watchCtx, model.MigrationSourceNodeLabelSelector(a.NodeName), "", events)
		}()
		func() {
			defer cancel()
			for {
				select {
				case <-ctx.Done():
					return
				case <-errCh:
					return
				case <-events:
					kick()
				}
			}
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
			// brief backoff before re-opening watches
		}
	}
}

func withBootResources(m model.Machine) model.Machine {
	size, err := hotplugpersist.Boot(m.Spec.Resources, m.Status.AppliedVCPUs, m.Status.AppliedMemoryMiB)
	if err != nil {
		return m
	}
	m.Spec.Resources.CPU = size.CPU
	m.Spec.Resources.Memory = size.Memory
	return m
}
