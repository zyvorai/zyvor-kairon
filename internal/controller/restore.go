// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// missingSnapshotGracePeriod bounds how long reconcileSnapshotRestore will
// park a restore Pending, rather than fail it outright, when its
// spec.snapshotName doesn't resolve to any MachineSnapshot yet. Sized like
// quiesceFreezeTimeout (snapshot.go) -- long enough to absorb an ordinary
// GitOps apply-ordering race (a MachineSnapshotRestore and its
// MachineSnapshot submitted together, the restore's own reconcile tick
// landing first), short enough that a genuinely missing/misspelled
// snapshotName still fails within a bounded, human-noticeable time rather
// than being silently Pending forever.
const missingSnapshotGracePeriod = 30 * time.Second

// reconcileSnapshotRestore restores one volume of a Succeeded
// MachineSnapshot into a brand-new PersistentVolumeClaim via the standard
// CSI spec.dataSource restore flow and, with spec.machine, creates a
// Machine booting from it (see model.MachineSnapshotRestore).
//
// The referenced MachineSnapshot (or its underlying VolumeSnapshot) simply
// not being ready *yet* is parked as Pending and retried next tick, not
// treated as an error -- the same "waiting on an external condition"
// posture reconcileVolumeSnapshots and finishRestoreFromPVCState already
// use elsewhere in this same feature. Before this, both cases returned a
// plain error, which the caller (Controller.Reconcile) turns into a
// permanent status.phase=Failed with no further retries -- so creating a
// MachineSnapshotRestore even slightly before its MachineSnapshot finished
// left it stuck Failed forever, even though the snapshot went on to
// succeed moments later.
//
// spec.snapshotName resolving to no MachineSnapshot at all gets the same
// grace-period treatment (missingSnapshotGracePeriod), bounded rather than
// open-ended: telling "will exist any moment now" (the exact same
// GitOps-apply-ordering race as above, just one step earlier -- the
// restore submitted alongside a MachineSnapshot that hasn't hit the API
// yet) apart from "will never exist" (a genuine typo) is only possible
// within a bounded window, using restore.Metadata.CreationTimestamp -- so
// it's treated as the former only while the restore itself is still
// nearly brand new, and as the latter (a real, permanent
// status.phase=Failed) once that window has passed.
func (c *Controller) reconcileSnapshotRestore(ctx context.Context, restore model.MachineSnapshotRestore) error {
	if restore.Status.Phase == "Succeeded" || restore.Status.Phase == "Failed" {
		return nil
	}
	if strings.TrimSpace(restore.Spec.SnapshotName) == "" || strings.TrimSpace(restore.Spec.TargetClaimName) == "" {
		return fmt.Errorf("spec.snapshotName and spec.targetClaimName are required")
	}
	snapshot, err := c.Kube.GetMachineSnapshot(ctx, restore.Namespace(), restore.Spec.SnapshotName)
	if err != nil {
		if kube.IsNotFound(err) && time.Since(restore.Metadata.CreationTimestamp) < missingSnapshotGracePeriod {
			return c.parkRestorePending(ctx, restore, fmt.Sprintf("waiting for MachineSnapshot %s to appear", restore.Spec.SnapshotName))
		}
		return fmt.Errorf("get MachineSnapshot %s: %w", restore.Spec.SnapshotName, err)
	}
	if snapshot.Status.Phase != "Succeeded" || !snapshot.Status.ReadyToUse {
		return c.parkRestorePending(ctx, restore, fmt.Sprintf("waiting for MachineSnapshot %s to become ready to restore from (phase=%q)", restore.Spec.SnapshotName, snapshot.Status.Phase))
	}
	ref, err := selectSnapshotVolume(snapshot.Status.VolumeSnapshots, restore.Spec.VolumeName)
	if err != nil {
		return err
	}
	if !ref.ReadyToUse {
		return c.parkRestorePending(ctx, restore, fmt.Sprintf("waiting for VolumeSnapshot %s to become ready to use", ref.VolumeSnapshotName))
	}

	var machineSpec *model.MachineSpec
	if restore.Spec.Machine != nil {
		if ref.AtlasSnapshotID != "" {
			return fmt.Errorf("spec.machine is not supported for Atlas snapshots")
		}
		spec, err := c.restoredMachineSpec(ctx, restore, snapshot, ref.VolumeName)
		if err != nil {
			return err
		}
		machineSpec = &spec
	}

	existing, err := c.Kube.GetPersistentVolumeClaim(ctx, restore.Namespace(), restore.Spec.TargetClaimName)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	if err == nil && existing.Metadata.Name != "" {
		if err := c.ensureRestoredMachine(ctx, &restore, snapshot, machineSpec); err != nil {
			return err
		}
		return c.finishRestoreFromPVCState(ctx, restore, existing)
	}
	if ref.AtlasSnapshotID != "" {
		return c.restoreFromAtlasSnapshot(ctx, restore, ref)
	}

	size := restore.Spec.StorageSize
	if size == "" {
		vs, err := c.Kube.GetVolumeSnapshot(ctx, restore.Namespace(), ref.VolumeSnapshotName)
		if err != nil {
			return fmt.Errorf("get VolumeSnapshot %s: %w", ref.VolumeSnapshotName, err)
		}
		if vs.Status.RestoreSize == nil || strings.TrimSpace(*vs.Status.RestoreSize) == "" {
			return fmt.Errorf("VolumeSnapshot %s reports no restoreSize; set spec.storageSize explicitly", ref.VolumeSnapshotName)
		}
		size = *vs.Status.RestoreSize
	}

	pvc := model.PersistentVolumeClaim{
		TypeMeta: model.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		Metadata: model.ObjectMeta{Name: restore.Spec.TargetClaimName, Namespace: restore.Namespace(), Labels: map[string]string{
			"kairon.zyvor.dev/restored-from-snapshot": snapshot.Metadata.Name,
		}},
		Spec: model.PersistentVolumeClaimSpec{
			AccessModes: []string{"ReadWriteOnce"},
			Resources:   &model.PersistentVolumeClaimResources{Requests: map[string]string{"storage": size}},
			DataSource:  &model.TypedLocalObjectReference{APIGroup: "snapshot.storage.k8s.io", Kind: "VolumeSnapshot", Name: ref.VolumeSnapshotName},
		},
	}
	if restore.Spec.StorageClassName != "" {
		pvc.Spec.StorageClassName = &restore.Spec.StorageClassName
	}
	created, err := c.Kube.CreatePersistentVolumeClaim(ctx, restore.Namespace(), pvc)
	if err != nil {
		return fmt.Errorf("create restored PersistentVolumeClaim %s: %w", restore.Spec.TargetClaimName, err)
	}
	// Created before the claim binds: under WaitForFirstConsumer the
	// Machine is what makes it bind.
	if err := c.ensureRestoredMachine(ctx, &restore, snapshot, machineSpec); err != nil {
		return err
	}
	return c.finishRestoreFromPVCState(ctx, restore, created)
}

// restoredMachineSpec is the spec of the Machine spec.machine asks for:
// the snapshot's recorded source spec or, for a snapshot taken before
// that was recorded, the source Machine as it is now.
func (c *Controller) restoredMachineSpec(ctx context.Context, restore model.MachineSnapshotRestore, snapshot model.MachineSnapshot, volumeName string) (model.MachineSpec, error) {
	if strings.TrimSpace(restore.Spec.Machine.Name) == "" {
		return model.MachineSpec{}, fmt.Errorf("spec.machine.name is required")
	}
	var source model.MachineSpec
	if snapshot.Status.SourceSpec != nil {
		source = *snapshot.Status.SourceSpec
	} else {
		m, err := c.Kube.GetMachine(ctx, snapshot.Namespace(), snapshot.Spec.MachineName)
		if err != nil {
			return model.MachineSpec{}, fmt.Errorf("snapshot %s has no recorded source spec and Machine %s is unavailable: %w", snapshot.Metadata.Name, snapshot.Spec.MachineName, err)
		}
		source = m.Spec
	}
	return model.RestoredMachineSpec(source, volumeName, restore.Spec.TargetClaimName, restore.Spec.Machine.PowerState)
}

// ensureRestoredMachine creates the spec.machine Machine once. An existing
// Machine of that name is accepted only if this restore created it.
func (c *Controller) ensureRestoredMachine(ctx context.Context, restore *model.MachineSnapshotRestore, snapshot model.MachineSnapshot, spec *model.MachineSpec) error {
	if spec == nil {
		return nil
	}
	name := restore.Spec.Machine.Name
	existing, err := c.Kube.GetMachine(ctx, restore.Namespace(), name)
	switch {
	case err == nil:
		if existing.Metadata.Labels[model.LabelRestoredBy] != restore.Metadata.Name {
			return fmt.Errorf("machine %s already exists and was not created by this restore", name)
		}
	case kube.IsNotFound(err):
		m := model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: "Machine"},
			Metadata: model.ObjectMeta{Name: name, Namespace: restore.Namespace(), Labels: map[string]string{
				model.LabelRestoredBy:                     restore.Metadata.Name,
				"kairon.zyvor.dev/restored-from-snapshot": snapshot.Metadata.Name,
			}},
			Spec: *spec,
		}
		if _, err := c.Kube.CreateMachine(ctx, restore.Namespace(), m); err != nil {
			return fmt.Errorf("create restored Machine %s: %w", name, err)
		}
		c.Log.Info("created restored machine", "namespace", restore.Namespace(), "restore", restore.Metadata.Name, "machine", name, "claim", restore.Spec.TargetClaimName)
	default:
		return err
	}
	restore.Status.RestoredMachineName = name
	return nil
}

// finishRestoreFromPVCState reports the restore as Succeeded once the
// target PVC is Bound. A freshly-created PVC under a WaitForFirstConsumer
// StorageClass stays Pending until something (a new Machine referencing
// it) is scheduled -- that's normal, not a failure, so an unbound PVC just
// leaves the restore Pending to be re-checked next reconcile tick, exactly
// like every other "waiting on an external condition" status in this
// project (e.g. MachineSnapshot itself waiting on its VolumeSnapshots).
func (c *Controller) finishRestoreFromPVCState(ctx context.Context, restore model.MachineSnapshotRestore, pvc model.PersistentVolumeClaim) error {
	status := restore.Status
	status.RestoredClaimName = pvc.Metadata.Name
	if pvc.Status.Phase == "Bound" {
		status.Phase = "Succeeded"
		status.Message = fmt.Sprintf("PersistentVolumeClaim %s is Bound and ready to use", pvc.Metadata.Name)
	} else {
		status.Phase = "Pending"
		status.Message = fmt.Sprintf("waiting for PersistentVolumeClaim %s to bind (phase=%q; normal under a WaitForFirstConsumer StorageClass until a Machine references it)", pvc.Metadata.Name, pvc.Status.Phase)
	}
	return c.Kube.PatchMachineSnapshotRestoreStatus(ctx, restore.Namespace(), restore.Metadata.Name, status)
}

// parkRestorePending records message as a Pending status and returns nil
// (not an error) -- so Controller.Reconcile's own per-item error handling
// never marks this restore Failed for what's actually a normal,
// automatically-retried wait, and never counts it against
// kairon_reconcile_item_errors_total{kind="snapshotrestore"} either.
func (c *Controller) parkRestorePending(ctx context.Context, restore model.MachineSnapshotRestore, message string) error {
	status := restore.Status
	status.Phase = "Pending"
	status.Message = message
	return c.Kube.PatchMachineSnapshotRestoreStatus(ctx, restore.Namespace(), restore.Metadata.Name, status)
}

func selectSnapshotVolume(refs []model.VolumeSnapshotReference, name string) (model.VolumeSnapshotReference, error) {
	if name == "" {
		if len(refs) == 1 {
			return refs[0], nil
		}
		return model.VolumeSnapshotReference{}, fmt.Errorf("spec.volumeName is required when the snapshot covers more than one volume")
	}
	for _, r := range refs {
		if r.VolumeName == name {
			return r, nil
		}
	}
	return model.VolumeSnapshotReference{}, fmt.Errorf("snapshot has no volume named %q", name)
}
