// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// backupJobs tracks disk backups and restores running in the background,
// keyed by "kind/namespace/name". A copy can take far longer than one
// reconcile tick, so it runs in its own goroutine and patches status when
// done; Reconcile only skips objects that are still in flight.
type backupJobs struct {
	mu      sync.Mutex
	running map[string]bool
}

func (j *backupJobs) start(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running[key] {
		return false
	}
	if j.running == nil {
		j.running = map[string]bool{}
	}
	j.running[key] = true
	return true
}

func (j *backupJobs) done(key string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.running, key)
}

func (j *backupJobs) busy(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running[key]
}

// backupJobTimeout bounds one background disk copy.
const backupJobTimeout = 2 * time.Hour

// reconcileBackups runs the disk half of MachineBackups and
// MachineBackupRestores whose status.nodeName is this node. The controller
// hands work over by setting status.disk Pending; this side owns status.disk
// from then on.
func (a *Agent) reconcileBackups(ctx context.Context, machines map[string]model.Machine) error {
	backups, err := a.Kube.ListMachineBackups(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, b := range backups {
		if b.Status.NodeName != a.NodeName || b.Status.Disk == nil {
			continue
		}
		if err := a.reconcileDiskBackup(ctx, b, machines); err != nil {
			a.Log.Error("machine backup failed", "namespace", b.Namespace(), "backup", b.Metadata.Name, "error", err)
		}
	}
	restores, err := a.Kube.ListMachineBackupRestores(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, r := range restores {
		if r.Status.NodeName != a.NodeName || r.Status.Disk == nil {
			continue
		}
		if err := a.reconcileDiskRestore(ctx, r, machines); err != nil {
			a.Log.Error("machine backup restore failed", "namespace", r.Namespace(), "restore", r.Metadata.Name, "error", err)
		}
	}
	return nil
}

func (a *Agent) patchDiskBackup(ctx context.Context, b model.MachineBackup, disk model.DiskBackupStatus) error {
	return a.Kube.PatchMachineBackupStatus(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"disk": disk})
}

func (a *Agent) reconcileDiskBackup(ctx context.Context, b model.MachineBackup, machines map[string]model.Machine) error {
	if b.IsCBT() {
		return a.reconcileCBTBackup(ctx, b, machines)
	}
	key := "backup/" + b.Namespace() + "/" + b.Metadata.Name
	if a.backups.busy(key) {
		return nil
	}
	disk := *b.Status.Disk
	if b.Metadata.DeletionTimestamp != nil {
		if !model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerFluxVMBackup) {
			return nil
		}
		if disk.Name != "" {
			if err := a.Flux.DeleteBackup(ctx, disk.Name); err != nil {
				return fmt.Errorf("delete FluxVM backup %s: %w", disk.Name, err)
			}
		}
		finals := model.RemoveFinalizer(b.Metadata.Finalizers, model.FinalizerFluxVMBackup)
		return a.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}})
	}

	switch disk.Phase {
	case model.BackupPending:
	case model.BackupRunning:
		// Not in flight here, so kairon-node restarted mid-copy. FluxVM
		// writes the metadata sidecar last, so created_at means it finished.
		return a.recoverDiskBackup(ctx, b, disk)
	default:
		return nil
	}

	m, ok := machines[b.Namespace()+"/"+b.Spec.MachineName]
	if !ok {
		disk.Phase, disk.Message = model.BackupFailed, "machine is no longer on this node"
		return a.patchDiskBackup(ctx, b, disk)
	}
	rec, err := a.current(ctx, m)
	if err != nil {
		disk.Phase, disk.Message = model.BackupFailed, fmt.Sprintf("find the FluxVM VM: %v", err)
		return a.patchDiskBackup(ctx, b, disk)
	}
	quiesce := b.Spec.Quiesce
	if quiesce == "" {
		quiesce = model.BackupQuiesceAuto
	}
	disk.Name = model.FluxVMBackupName(b)
	disk.Phase, disk.Message = model.BackupRunning, "copying disks"
	if err := a.patchDiskBackup(ctx, b, disk); err != nil {
		return err
	}
	if !a.backups.start(key) {
		return nil
	}
	id := rec.ID()
	go func() {
		defer a.backups.done(key)
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupJobTimeout)
		defer cancel()
		out, err := a.Flux.BackupVM(jobCtx, id, disk.Name, quiesce)
		if err != nil {
			disk.Phase, disk.Message = model.BackupFailed, err.Error()
		} else {
			fillDiskBackup(&disk, out)
		}
		if err := a.patchDiskBackup(jobCtx, b, disk); err != nil {
			a.Log.Error("machine backup status patch failed", "namespace", b.Namespace(), "backup", b.Metadata.Name, "error", err)
		}
	}()
	return nil
}

func fillDiskBackup(disk *model.DiskBackupStatus, out fluxvm.Backup) {
	disk.Phase, disk.Message = model.BackupSucceeded, ""
	disk.SizeBytes, disk.Quiesced = out.SizeBytes, out.Quiesced
	disk.Disks = nil
	for _, d := range out.Disks {
		disk.Disks = append(disk.Disks, d.Name)
	}
	if !out.Quiesced {
		disk.Message = "crash-consistent: the guest agent did not freeze filesystems"
	}
}

func (a *Agent) recoverDiskBackup(ctx context.Context, b model.MachineBackup, disk model.DiskBackupStatus) error {
	list, err := a.Flux.ListBackups(ctx)
	if err != nil {
		return err
	}
	for _, item := range list {
		if item.Name != disk.Name {
			continue
		}
		if item.CreatedAt != "" {
			fillDiskBackup(&disk, item)
			return a.patchDiskBackup(ctx, b, disk)
		}
		if err := a.Flux.DeleteBackup(ctx, disk.Name); err != nil {
			return err
		}
	}
	disk.Phase, disk.Message = model.BackupFailed, "kairon-node restarted before the backup finished; create a new MachineBackup"
	return a.patchDiskBackup(ctx, b, disk)
}

func (a *Agent) patchDiskRestore(ctx context.Context, r model.MachineBackupRestore, disk model.DiskBackupStatus) error {
	return a.Kube.PatchMachineBackupRestoreStatus(ctx, r.Namespace(), r.Metadata.Name, map[string]any{"disk": disk})
}

func (a *Agent) reconcileDiskRestore(ctx context.Context, r model.MachineBackupRestore, machines map[string]model.Machine) error {
	key := "restore/" + r.Namespace() + "/" + r.Metadata.Name
	if a.backups.busy(key) {
		return nil
	}
	disk := *r.Status.Disk
	if disk.ManifestKey != "" {
		return a.reconcileCBTRestore(ctx, r, disk, machines)
	}
	switch disk.Phase {
	case model.BackupPending:
	case model.BackupRunning:
		disk.Phase, disk.Message = model.BackupFailed, "kairon-node restarted during the restore; the Machine's disks may be partly restored, so restore again before starting it"
		return a.patchDiskRestore(ctx, r, disk)
	default:
		return nil
	}
	m, ok := machines[r.Namespace()+"/"+r.Status.MachineName]
	if !ok {
		disk.Phase, disk.Message = model.BackupFailed, "machine is no longer on this node"
		return a.patchDiskRestore(ctx, r, disk)
	}
	rec, err := a.current(ctx, m)
	if err != nil {
		disk.Phase, disk.Message = model.BackupFailed, fmt.Sprintf("find the FluxVM VM: %v", err)
		return a.patchDiskRestore(ctx, r, disk)
	}
	if rec == nil || rec.ID() == "" {
		disk.Phase, disk.Message = model.BackupFailed, "the Machine has no FluxVM VM to restore into (powerState Stopped deletes it; use Halted)"
		return a.patchDiskRestore(ctx, r, disk)
	}
	if m.DesiredPowerState() != "Halted" || normalizePhase(rec.Status) != "Stopped" {
		msg := "waiting for the Machine to be halted (spec.powerState: Halted)"
		if disk.Message == msg {
			return nil
		}
		disk.Message = msg
		return a.patchDiskRestore(ctx, r, disk)
	}
	disk.Phase, disk.Message = model.BackupRunning, "copying disks"
	if err := a.patchDiskRestore(ctx, r, disk); err != nil {
		return err
	}
	if !a.backups.start(key) {
		return nil
	}
	id := rec.ID()
	go func() {
		defer a.backups.done(key)
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupJobTimeout)
		defer cancel()
		restored, err := a.Flux.RestoreBackup(jobCtx, id, disk.Name)
		if err != nil {
			disk.Phase, disk.Message = model.BackupFailed, err.Error()
		} else {
			disk.Phase, disk.Message, disk.Disks = model.BackupSucceeded, "", restored
		}
		if err := a.patchDiskRestore(jobCtx, r, disk); err != nil {
			a.Log.Error("machine backup restore status patch failed", "namespace", r.Namespace(), "restore", r.Metadata.Name, "error", err)
		}
	}()
	return nil
}
