// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileMachineBackups drives every MachineBackup and
// MachineBackupRestore. The disk half of each runs on kairon-node (see
// internal/agent/backup.go); this side decides which halves apply, runs
// the Atlas jobs and folds both into status.phase.
func (c *Controller) reconcileMachineBackups(ctx context.Context, machines map[string]model.Machine) error {
	backups, err := c.Kube.ListMachineBackups(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	for _, b := range backups {
		if err := c.reconcileMachineBackup(ctx, b, machines); err != nil {
			c.Log.Error("machine backup reconcile failed", "namespace", b.Namespace(), "backup", b.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("machinebackup")
			}
			if transientAPIError(err) {
				continue
			}
			c.patchBackupStatus(ctx, b, map[string]any{"phase": model.BackupFailed, "message": err.Error(), "completionTime": time.Now().UTC()})
		}
	}
	restores, err := c.Kube.ListMachineBackupRestores(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	for _, r := range restores {
		if err := c.reconcileMachineBackupRestore(ctx, r, machines); err != nil {
			c.Log.Error("machine backup restore reconcile failed", "namespace", r.Namespace(), "restore", r.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("machinebackuprestore")
			}
			if transientAPIError(err) {
				continue
			}
			c.patchRestoreStatus(ctx, r, map[string]any{"phase": model.BackupFailed, "message": err.Error(), "completionTime": time.Now().UTC()})
		}
	}
	return nil
}

func (c *Controller) patchBackupStatus(ctx context.Context, b model.MachineBackup, status map[string]any) {
	if err := c.Kube.PatchMachineBackupStatus(ctx, b.Namespace(), b.Metadata.Name, status); err != nil {
		c.Log.Error("machine backup status patch failed", "namespace", b.Namespace(), "backup", b.Metadata.Name, "error", err)
	}
}

func (c *Controller) patchRestoreStatus(ctx context.Context, r model.MachineBackupRestore, status map[string]any) {
	if err := c.Kube.PatchMachineBackupRestoreStatus(ctx, r.Namespace(), r.Metadata.Name, status); err != nil {
		c.Log.Error("machine backup restore status patch failed", "namespace", r.Namespace(), "restore", r.Metadata.Name, "error", err)
	}
}

func (c *Controller) reconcileMachineBackup(ctx context.Context, b model.MachineBackup, machines map[string]model.Machine) error {
	if b.Metadata.DeletionTimestamp != nil {
		// kairon-node deletes a FluxVM backup it wrote; if it never got a
		// name there is nothing on any node to delete.
		if model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerFluxVMBackup) && (b.Status.Disk == nil || b.Status.Disk.Name == "") {
			finals := model.RemoveFinalizer(b.Metadata.Finalizers, model.FinalizerFluxVMBackup)
			return c.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}})
		}
		if model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerCBTBackup) && !cbtWroteAnything(b) {
			finals := model.RemoveFinalizer(b.Metadata.Finalizers, model.FinalizerCBTBackup)
			return c.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}})
		}
		return nil
	}
	// A restore point imported from a DR peer was taken there.
	if b.Replicated() {
		return nil
	}
	switch b.Status.Phase {
	case model.BackupSucceeded, model.BackupFailed:
		return nil
	case "":
		return c.startMachineBackup(ctx, b, machines)
	}

	volumes := append([]model.VolumeBackupStatus(nil), b.Status.Volumes...)
	if len(volumes) > 0 {
		machine := machines[b.Namespace()+"/"+b.Spec.MachineName]
		states := model.AtlasVolumeStates(machine)
		bucket := c.backupBucket(b)
		for i := range volumes {
			c.stepAtlasBackup(ctx, b, &volumes[i], states[volumes[i].VolumeName].VolumeID, bucket)
		}
	}
	phase, message := model.BackupHalvesDone(b.Status.Disk, volumes)
	if message == "" {
		message = backupProgress(phase, b.Status.Disk, volumes)
	}
	if phase == b.Status.Phase && message == b.Status.Message && reflect.DeepEqual(volumes, b.Status.Volumes) {
		return nil
	}
	status := map[string]any{"phase": phase, "message": message, "volumes": volumes}
	if phase != model.BackupRunning {
		status["completionTime"] = time.Now().UTC()
	}
	if phase == model.BackupSucceeded && b.IsCBT() {
		promoteCBTResult(b, status)
	}
	if c.Metrics != nil && phase != model.BackupRunning && b.IsCBT() && b.Status.Disk != nil {
		c.Metrics.ObserveCBTBackup(b, phase)
	}
	return c.Kube.PatchMachineBackupStatus(ctx, b.Namespace(), b.Metadata.Name, status)
}

func (c *Controller) backupBucket(b model.MachineBackup) string {
	if b.Spec.Atlas != nil && b.Spec.Atlas.BucketID != "" {
		return b.Spec.Atlas.BucketID
	}
	return c.Atlas.BackupBucketID
}

// startMachineBackup decides which halves apply and hands the disk half to
// kairon-node by setting status.disk Pending.
func (c *Controller) startMachineBackup(ctx context.Context, b model.MachineBackup, machines map[string]model.Machine) error {
	if strings.TrimSpace(b.Spec.MachineName) == "" {
		return fmt.Errorf("spec.machineName is required")
	}
	if !model.ValidBackupQuiesce(b.Spec.Quiesce) {
		return fmt.Errorf("spec.quiesce must be auto, required or never")
	}
	machine, ok := machines[b.Namespace()+"/"+b.Spec.MachineName]
	if !ok {
		return fmt.Errorf("machine %s/%s not found", b.Namespace(), b.Spec.MachineName)
	}
	status := map[string]any{"phase": model.BackupRunning}
	var parts []string

	if b.IsCBT() {
		if err := c.startCBTBackup(ctx, b, machine, status); err != nil {
			return err
		}
		parts = append(parts, fmt.Sprintf("%s disks to s3://%s", b.BackupMode(), b.Spec.Target.S3.Bucket))
	} else if err := model.ValidateCBTBackup(b); err != nil {
		return err
	}

	// FluxVM owns the root disk only when the Machine boots from an image.
	if len(machine.Spec.Volumes) == 0 && !b.IsCBT() {
		if machine.Spec.NodeName == "" {
			return fmt.Errorf("machine %s is not scheduled to a node yet", machine.Metadata.Name)
		}
		if !model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerFluxVMBackup) {
			finals := append(append([]string{}, b.Metadata.Finalizers...), model.FinalizerFluxVMBackup)
			if err := c.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
				return fmt.Errorf("add backup finalizer: %w", err)
			}
		}
		status["nodeName"] = machine.Spec.NodeName
		status["disk"] = model.DiskBackupStatus{Phase: model.BackupPending, Message: "waiting for kairon-node on " + machine.Spec.NodeName}
		parts = append(parts, "disks on "+machine.Spec.NodeName)
	}

	if b.Spec.Atlas != nil {
		if c.Atlas.Client == nil {
			return fmt.Errorf("spec.atlas is set but Atlas is not configured on kairon-controller")
		}
		if c.backupBucket(b) == "" {
			return fmt.Errorf("spec.atlas.bucketID is empty and kairon-controller has no --atlas-backup-bucket")
		}
		volumes, err := atlasBackupVolumes(b, machine)
		if err != nil {
			return err
		}
		status["volumes"] = volumes
		parts = append(parts, fmt.Sprintf("%d Atlas volume(s)", len(volumes)))
	}

	if len(parts) == 0 {
		return fmt.Errorf("nothing to back up: machine %s boots from spec.volumes (use MachineSnapshot) and spec.atlas is unset", machine.Metadata.Name)
	}
	status["message"] = "backing up " + strings.Join(parts, " and ")
	return c.Kube.PatchMachineBackupStatus(ctx, b.Namespace(), b.Metadata.Name, status)
}

func atlasBackupVolumes(b model.MachineBackup, machine model.Machine) ([]model.VolumeBackupStatus, error) {
	want := map[string]bool{}
	for _, n := range b.Spec.Atlas.VolumeNames {
		want[n] = true
	}
	var out []model.VolumeBackupStatus
	for _, v := range machine.Spec.Volumes {
		if v.Atlas == nil || (len(want) > 0 && !want[v.Name]) {
			continue
		}
		delete(want, v.Name)
		out = append(out, model.VolumeBackupStatus{VolumeName: v.Name, Phase: model.BackupPending})
	}
	for n := range want {
		return nil, fmt.Errorf("spec.atlas.volumeNames: machine %s has no Atlas volume %q", machine.Metadata.Name, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("spec.atlas is set but machine %s has no Atlas volumes", machine.Metadata.Name)
	}
	return out, nil
}

// stepAtlasBackup submits or polls one Atlas backup job. Failures land in
// the volume's own status so the other halves keep going.
func (c *Controller) stepAtlasBackup(ctx context.Context, b model.MachineBackup, v *model.VolumeBackupStatus, volumeID, bucket string) {
	if v.Phase == model.BackupSucceeded || v.Phase == model.BackupFailed {
		return
	}
	if c.Atlas.Client == nil {
		v.Phase, v.Message = model.BackupFailed, "Atlas is not configured on kairon-controller"
		return
	}
	if v.AtlasJobID == "" {
		if volumeID == "" {
			v.Phase, v.Message = model.BackupFailed, "Atlas volume is not provisioned"
			return
		}
		req := atlas.BackupRequest{VolumeID: volumeID, BucketID: bucket}
		if b.Spec.Atlas != nil {
			req.Keep, req.MaxAgeSecs = b.Spec.Atlas.Keep, b.Spec.Atlas.MaxAgeSeconds
		}
		acc, err := c.Atlas.Client.CreateBackup(ctx, req)
		if err != nil {
			v.Phase, v.Message = model.BackupFailed, fmt.Sprintf("submit Atlas backup: %v", err)
			return
		}
		v.AtlasJobID, v.AtlasBackupID, v.Phase = acc.JobID, acc.Resource.BackupID, model.BackupRunning
		if acc.State == atlas.JobSucceeded && v.AtlasBackupID != "" {
			v.Phase = model.BackupSucceeded
		}
		return
	}
	stepAtlasJob(ctx, c.Atlas.Client, v, func(r atlas.Resource) {
		if v.AtlasBackupID == "" {
			v.AtlasBackupID = r.BackupID
		}
	})
}

// stepAtlasJob polls v.AtlasJobID and records the outcome. A transient
// error leaves the volume Running for the next tick.
func stepAtlasJob(ctx context.Context, client *atlas.Client, v *model.VolumeBackupStatus, onSuccess func(atlas.Resource)) {
	job, err := client.GetJob(ctx, v.AtlasJobID)
	if err != nil {
		v.Message = fmt.Sprintf("checking Atlas job %s: %v", v.AtlasJobID, err)
		return
	}
	switch job.State {
	case atlas.JobSucceeded:
		onSuccess(jobResource(job))
		v.Phase, v.Message = model.BackupSucceeded, ""
	case atlas.JobFailed:
		msg := "failed"
		if job.Error != nil {
			msg = *job.Error
		}
		v.Phase, v.Message = model.BackupFailed, fmt.Sprintf("Atlas job %s: %s", job.ID, msg)
	default:
		v.Message = fmt.Sprintf("Atlas job %s %s", job.ID, job.State)
	}
}

func backupProgress(phase string, disk *model.DiskBackupStatus, volumes []model.VolumeBackupStatus) string {
	if phase == model.BackupSucceeded {
		var parts []string
		if disk != nil {
			parts = append(parts, fmt.Sprintf("disks in FluxVM backup %s", disk.Name))
		}
		if len(volumes) > 0 {
			parts = append(parts, fmt.Sprintf("%d Atlas volume(s)", len(volumes)))
		}
		return "completed: " + strings.Join(parts, ", ")
	}
	var waiting []string
	if disk != nil && disk.Phase != model.BackupSucceeded {
		waiting = append(waiting, "disks ("+strings.ToLower(disk.Phase)+")")
	}
	for _, v := range volumes {
		if v.Phase != model.BackupSucceeded {
			waiting = append(waiting, "volume "+v.VolumeName+" ("+strings.ToLower(v.Phase)+")")
		}
	}
	return "waiting for " + strings.Join(waiting, ", ")
}

func (c *Controller) reconcileMachineBackupRestore(ctx context.Context, r model.MachineBackupRestore, machines map[string]model.Machine) error {
	switch r.Status.Phase {
	case model.BackupSucceeded, model.BackupFailed:
		return nil
	case "", model.BackupPending:
		return c.startMachineBackupRestore(ctx, r, machines)
	}
	volumes := append([]model.VolumeBackupStatus(nil), r.Status.Volumes...)
	for i := range volumes {
		c.stepAtlasRestore(ctx, r, &volumes[i])
	}
	phase, message := model.BackupHalvesDone(r.Status.Disk, volumes)
	if message == "" {
		message = backupProgress(phase, r.Status.Disk, volumes)
		if phase == model.BackupSucceeded {
			message = "restored"
		}
	}
	if phase == r.Status.Phase && message == r.Status.Message && reflect.DeepEqual(volumes, r.Status.Volumes) {
		return nil
	}
	status := map[string]any{"phase": phase, "message": message, "volumes": volumes}
	if phase != model.BackupRunning {
		status["completionTime"] = time.Now().UTC()
	}
	return c.Kube.PatchMachineBackupRestoreStatus(ctx, r.Namespace(), r.Metadata.Name, status)
}

func (c *Controller) startMachineBackupRestore(ctx context.Context, r model.MachineBackupRestore, machines map[string]model.Machine) error {
	if strings.TrimSpace(r.Spec.BackupName) == "" {
		return fmt.Errorf("spec.backupName is required")
	}
	b, err := c.Kube.GetMachineBackup(ctx, r.Namespace(), r.Spec.BackupName)
	if err != nil {
		if kube.IsNotFound(err) && time.Since(r.Metadata.CreationTimestamp) < missingSnapshotGracePeriod {
			return c.parkBackupRestore(ctx, r, fmt.Sprintf("waiting for MachineBackup %s to appear", r.Spec.BackupName))
		}
		return fmt.Errorf("get MachineBackup %s: %w", r.Spec.BackupName, err)
	}
	switch b.Status.Phase {
	case model.BackupSucceeded:
	case model.BackupFailed:
		return fmt.Errorf("MachineBackup %s failed: %s", b.Metadata.Name, b.Status.Message)
	default:
		return c.parkBackupRestore(ctx, r, fmt.Sprintf("waiting for MachineBackup %s to succeed (phase %q)", b.Metadata.Name, b.Status.Phase))
	}
	machineName := r.Spec.MachineName
	if machineName == "" {
		machineName = b.Spec.MachineName
	}
	status := map[string]any{"phase": model.BackupRunning, "machineName": machineName}
	var parts []string
	if b.IsCBT() && b.Status.ManifestKey != "" {
		machine, ok := machines[r.Namespace()+"/"+machineName]
		if !ok {
			return fmt.Errorf("machine %s/%s not found", r.Namespace(), machineName)
		}
		if err := c.cbtNodeReady(ctx, machine.Spec.NodeName); err != nil {
			return err
		}
		bases, err := c.syntheticBases(ctx, b)
		if err != nil {
			return err
		}
		status["nodeName"] = machine.Spec.NodeName
		status["disk"] = model.DiskBackupStatus{
			Phase:          model.BackupPending,
			Name:           b.Metadata.Name,
			ManifestKey:    b.Status.ManifestKey,
			SyntheticBases: bases,
			Message:        "waiting for kairon-node on " + machine.Spec.NodeName,
		}
		parts = append(parts, "disks from s3 into machine "+machineName)
	} else if b.Status.Disk != nil && b.Status.Disk.Name != "" {
		machine, ok := machines[r.Namespace()+"/"+machineName]
		if !ok {
			return fmt.Errorf("machine %s/%s not found", r.Namespace(), machineName)
		}
		if machine.Spec.NodeName != b.Status.NodeName {
			return fmt.Errorf("the disk backup is stored on node %q but machine %s runs on %q; FluxVM backups stay on the node that wrote them", b.Status.NodeName, machineName, machine.Spec.NodeName)
		}
		status["nodeName"] = b.Status.NodeName
		status["disk"] = model.DiskBackupStatus{Phase: model.BackupPending, Name: b.Status.Disk.Name, Message: "waiting for kairon-node on " + b.Status.NodeName}
		parts = append(parts, "disks into machine "+machineName)
	}
	var volumes []model.VolumeBackupStatus
	for _, v := range b.Status.Volumes {
		if v.AtlasBackupID == "" {
			continue
		}
		volumes = append(volumes, model.VolumeBackupStatus{VolumeName: v.VolumeName, AtlasBackupID: v.AtlasBackupID, Phase: model.BackupPending})
	}
	if len(volumes) > 0 {
		if c.Atlas.Client == nil {
			return fmt.Errorf("the backup has Atlas volumes but Atlas is not configured on kairon-controller")
		}
		status["volumes"] = volumes
		parts = append(parts, fmt.Sprintf("%d Atlas volume(s) into new volumes", len(volumes)))
	}
	if len(parts) == 0 {
		return fmt.Errorf("MachineBackup %s holds nothing to restore", b.Metadata.Name)
	}
	status["message"] = "restoring " + strings.Join(parts, " and ")
	return c.Kube.PatchMachineBackupRestoreStatus(ctx, r.Namespace(), r.Metadata.Name, status)
}

func (c *Controller) parkBackupRestore(ctx context.Context, r model.MachineBackupRestore, message string) error {
	if r.Status.Phase == model.BackupPending && r.Status.Message == message {
		return nil
	}
	return c.Kube.PatchMachineBackupRestoreStatus(ctx, r.Namespace(), r.Metadata.Name, map[string]any{"phase": model.BackupPending, "message": message})
}

func (c *Controller) stepAtlasRestore(ctx context.Context, r model.MachineBackupRestore, v *model.VolumeBackupStatus) {
	if v.Phase == model.BackupSucceeded || v.Phase == model.BackupFailed {
		return
	}
	if c.Atlas.Client == nil {
		v.Phase, v.Message = model.BackupFailed, "Atlas is not configured on kairon-controller"
		return
	}
	if v.AtlasJobID == "" {
		acc, err := c.Atlas.Client.CreateRestore(ctx, atlas.RestoreRequest{
			BackupID:     v.AtlasBackupID,
			Name:         model.RestoredVolumeName(r, v.VolumeName),
			StorageClass: r.Spec.StorageClassName,
			Mode:         atlas.RestoreData,
		})
		if err != nil {
			v.Phase, v.Message = model.BackupFailed, fmt.Sprintf("submit Atlas restore: %v", err)
			return
		}
		v.AtlasJobID, v.Phase = acc.JobID, model.BackupRunning
		v.AtlasVolumeID, v.ClaimName = acc.Resource.VolumeID, acc.Resource.PVC
		return
	}
	stepAtlasJob(ctx, c.Atlas.Client, v, func(res atlas.Resource) {
		if res.VolumeID != "" {
			v.AtlasVolumeID = res.VolumeID
		}
		if res.PVC != "" {
			v.ClaimName = res.PVC
		}
	})
}
