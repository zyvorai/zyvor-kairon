// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// atlasSnapshotName names the Atlas snapshot of one volume in one
// MachineSnapshot. The "snap-" prefix keeps it apart from the volume names
// model.AtlasVolumeName gives Machines.
func atlasSnapshotName(snapshot model.MachineSnapshot, volume string) string {
	return model.AtlasVolumeName(snapshot.Namespace(), "snap-"+snapshot.Metadata.Name, volume)
}

func usesAtlasSnapshots(c *Controller, machine model.Machine) bool {
	if c.Atlas.Client == nil {
		return false
	}
	for _, v := range machine.Spec.Volumes {
		if v.Atlas != nil {
			return true
		}
	}
	return false
}

// ensureAtlasSnapshotFinalizer adds FinalizerAtlasSnapshots before the
// first Atlas snapshot is requested, so deleting the MachineSnapshot can
// delete them.
func (c *Controller) ensureAtlasSnapshotFinalizer(ctx context.Context, snapshot *model.MachineSnapshot, machine model.Machine) error {
	if !usesAtlasSnapshots(c, machine) || model.HasFinalizerList(snapshot.Metadata.Finalizers, model.FinalizerAtlasSnapshots) {
		return nil
	}
	finals := append(append([]string{}, snapshot.Metadata.Finalizers...), model.FinalizerAtlasSnapshots)
	patch := map[string]any{"metadata": map[string]any{"finalizers": finals}}
	if err := c.Kube.PatchMachineSnapshot(ctx, snapshot.Namespace(), snapshot.Metadata.Name, patch); err != nil {
		return fmt.Errorf("add atlas finalizer to snapshot %s/%s: %w", snapshot.Namespace(), snapshot.Metadata.Name, err)
	}
	snapshot.Metadata.Finalizers = finals
	return nil
}

// stepAtlasSnapshot requests or polls the Atlas snapshot of one volume.
// created reports whether this call issued the snapshot request.
func (c *Controller) stepAtlasSnapshot(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine, volume model.MachineVolume) (ref model.VolumeSnapshotReference, created bool, err error) {
	ref = model.VolumeSnapshotReference{VolumeName: volume.Name, VolumeSnapshotName: atlasSnapshotName(snapshot, volume.Name)}
	for _, prev := range snapshot.Status.VolumeSnapshots {
		if prev.VolumeName == volume.Name && prev.AtlasJobID != "" {
			ref = prev
		}
	}
	if ref.ReadyToUse {
		return ref, false, nil
	}

	if ref.AtlasJobID == "" {
		st := model.AtlasVolumeStates(machine)[volume.Name]
		if st.Phase != model.AtlasPhaseReady || st.VolumeID == "" {
			return ref, false, fmt.Errorf("atlas volume %q is not Ready (phase %q)", volume.Name, st.Phase)
		}
		acc, err := c.Atlas.Client.CreateSnapshot(ctx, st.VolumeID, atlas.SnapshotRequest{Name: ref.VolumeSnapshotName})
		if err != nil {
			return ref, false, fmt.Errorf("atlas snapshot of volume %q: %w", volume.Name, err)
		}
		ref.AtlasJobID, ref.AtlasSnapshotID = acc.JobID, acc.Resource.SnapshotID
		ref.ReadyToUse = acc.State == atlas.JobSucceeded && ref.AtlasSnapshotID != ""
		return ref, true, nil
	}

	job, err := c.Atlas.Client.GetJob(ctx, ref.AtlasJobID)
	if err != nil {
		return ref, false, fmt.Errorf("get atlas job %s: %w", ref.AtlasJobID, err)
	}
	switch job.State {
	case atlas.JobSucceeded:
		if ref.AtlasSnapshotID == "" {
			ref.AtlasSnapshotID = jobResource(job).SnapshotID
		}
		if ref.AtlasSnapshotID == "" {
			return ref, false, fmt.Errorf("atlas snapshot job %s succeeded without a snapshot id", job.ID)
		}
		ref.ReadyToUse = true
	case atlas.JobFailed:
		msg := "failed"
		if job.Error != nil {
			msg = *job.Error
		}
		return ref, false, fmt.Errorf("atlas snapshot job %s for volume %q: %s", job.ID, volume.Name, msg)
	}
	return ref, false, nil
}

func jobResource(job atlas.Job) atlas.Resource {
	var r atlas.Resource
	if len(job.Result) > 0 {
		_ = json.Unmarshal(job.Result, &r)
	}
	return r
}

// failSnapshot records refs together with Failed, so Atlas snapshots that
// were already requested stay listed in status and get deleted with the
// MachineSnapshot.
func (c *Controller) failSnapshot(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine, refs []model.VolumeSnapshotReference, cause error) error {
	if snapshot.Status.Phase == "Freezing" {
		clear := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationQuiesceRequest: nil}}}
		if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, clear); err != nil {
			return fmt.Errorf("thaw machine %s/%s after failed snapshot: %w", machine.Namespace(), machine.Metadata.Name, err)
		}
	}
	status := snapshot.Status
	status.VolumeSnapshots = refs
	status.Phase = "Failed"
	status.ReadyToUse = false
	status.Message = cause.Error()
	c.Log.Error("snapshot reconcile failed", "namespace", snapshot.Namespace(), "snapshot", snapshot.Metadata.Name, "error", cause)
	if c.Metrics != nil {
		c.Metrics.ObserveReconcileItemError("snapshot")
	}
	return c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status)
}

// releaseAtlasSnapshots deletes every Atlas snapshot recorded in status and
// drops FinalizerAtlasSnapshots. A snapshot Atlas refuses to delete because
// volumes were cloned from it (409) is left in Atlas and logged rather than
// blocking the MachineSnapshot's deletion forever.
func (c *Controller) releaseAtlasSnapshots(ctx context.Context, snapshot *model.MachineSnapshot) error {
	if c.Atlas.Client == nil {
		return fmt.Errorf("snapshot %s/%s has Atlas snapshots but Atlas is not configured", snapshot.Namespace(), snapshot.Metadata.Name)
	}
	for _, ref := range snapshot.Status.VolumeSnapshots {
		id := ref.AtlasSnapshotID
		if id == "" && ref.AtlasJobID != "" {
			job, err := c.Atlas.Client.GetJob(ctx, ref.AtlasJobID)
			if err != nil && !atlas.IsNotFound(err) {
				return fmt.Errorf("get atlas job %s: %w", ref.AtlasJobID, err)
			}
			if err == nil && !job.Terminal() {
				return nil // wait for the snapshot to exist before deleting it
			}
			id = jobResource(job).SnapshotID
		}
		if id == "" {
			continue
		}
		_, err := c.Atlas.Client.DeleteSnapshot(ctx, id, false)
		switch {
		case err == nil, atlas.IsNotFound(err):
		case atlas.IsConflict(err):
			c.Log.Warn("atlas snapshot has clones; leaving it in Atlas", "namespace", snapshot.Namespace(), "snapshot", snapshot.Metadata.Name, "atlasSnapshot", id)
		default:
			return fmt.Errorf("delete atlas snapshot %s: %w", id, err)
		}
	}
	finals := model.RemoveFinalizer(snapshot.Metadata.Finalizers, model.FinalizerAtlasSnapshots)
	patch := map[string]any{"metadata": map[string]any{"finalizers": finals}}
	if err := c.Kube.PatchMachineSnapshot(ctx, snapshot.Namespace(), snapshot.Metadata.Name, patch); err != nil {
		return err
	}
	snapshot.Metadata.Finalizers = finals
	return nil
}

// restoreFromAtlasSnapshot restores an Atlas-backed snapshot volume into
// restore.spec.targetClaimName through Atlas, which creates the PVC.
func (c *Controller) restoreFromAtlasSnapshot(ctx context.Context, restore model.MachineSnapshotRestore, ref model.VolumeSnapshotReference) error {
	if c.Atlas.Client == nil {
		return fmt.Errorf("snapshot volume %q is an Atlas snapshot but Atlas is not configured", ref.VolumeName)
	}
	if restore.Status.AtlasJobID == "" {
		var size int64
		if restore.Spec.StorageSize != "" {
			var err error
			if size, err = model.ParseBytes(restore.Spec.StorageSize); err != nil {
				return fmt.Errorf("spec.storageSize: %w", err)
			}
		}
		acc, err := c.Atlas.Client.RestoreSnapshot(ctx, ref.AtlasSnapshotID, atlas.CloneRequest{
			Name:         restore.Spec.TargetClaimName,
			Namespace:    restore.Namespace(),
			StorageClass: restore.Spec.StorageClassName,
			SizeBytes:    size,
			Owner: &atlas.Owner{
				Product: "kairon", ResourceType: "machinesnapshotrestore",
				ResourceID: restore.Namespace() + "/" + restore.Metadata.Name,
			},
		})
		if err != nil {
			return fmt.Errorf("atlas restore of snapshot %s: %w", ref.AtlasSnapshotID, err)
		}
		status := restore.Status
		status.AtlasJobID = acc.JobID
		status.Phase = "Pending"
		status.Message = fmt.Sprintf("Atlas restore job %s submitted", acc.JobID)
		return c.Kube.PatchMachineSnapshotRestoreStatus(ctx, restore.Namespace(), restore.Metadata.Name, status)
	}
	job, err := c.Atlas.Client.GetJob(ctx, restore.Status.AtlasJobID)
	if err != nil {
		return c.parkRestorePending(ctx, restore, fmt.Sprintf("checking Atlas restore job %s: %v", restore.Status.AtlasJobID, err))
	}
	switch job.State {
	case atlas.JobFailed:
		msg := "failed"
		if job.Error != nil {
			msg = *job.Error
		}
		return fmt.Errorf("atlas restore job %s: %s", job.ID, msg)
	case atlas.JobSucceeded:
		pvc, err := c.Kube.GetPersistentVolumeClaim(ctx, restore.Namespace(), restore.Spec.TargetClaimName)
		if kube.IsNotFound(err) {
			return c.parkRestorePending(ctx, restore, fmt.Sprintf("Atlas restore succeeded; waiting for PersistentVolumeClaim %s", restore.Spec.TargetClaimName))
		}
		if err != nil {
			return err
		}
		return c.finishRestoreFromPVCState(ctx, restore, pvc)
	}
	return c.parkRestorePending(ctx, restore, fmt.Sprintf("waiting for Atlas restore job %s (%s)", job.ID, job.State))
}
