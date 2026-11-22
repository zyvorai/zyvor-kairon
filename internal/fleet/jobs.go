// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fleet

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func (e *Engine) backupGroup(ctx context.Context, o *model.FleetResource) error {
	s, _ := decode[BackupGroupSpec](o.Spec)
	now := e.now()
	if o.Status.LastActionTime == nil {
		// Persist the batch identity before creating children. A restart can replay
		// every deterministic child name without creating duplicate backups.
		o.Status.LastActionTime = &now
		e.status(o, "Pending", "backup batch reserved")
		return nil
	}
	batch := o.Status.LastActionTime.Unix()
	allDone := true
	for _, name := range s.Machines {
		backupName := childName(*o, "backup/"+name+"/"+strconv.FormatInt(batch, 10))
		b, err := e.Kube.GetMachineBackup(ctx, o.Metadata.Namespace, backupName)
		if kube.IsNotFound(err) {
			// Preflight each Machine before handing work to the existing backup engine.
			m, getErr := e.Kube.GetMachine(ctx, o.Metadata.Namespace, name)
			if getErr != nil {
				return getErr
			}
			if m.Status.Phase != "Running" {
				return fmt.Errorf("backup Machine %s is not Running", name)
			}
			spec := model.MachineBackupSpec{MachineName: name, Quiesce: s.Quiesce}
			if s.AtlasBucketID != "" {
				spec.Atlas = &model.MachineBackupAtlas{BucketID: s.AtlasBucketID}
			}
			b, err = e.Kube.CreateMachineBackup(ctx, o.Metadata.Namespace, model.MachineBackup{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup}, Metadata: childMeta(*o, backupName), Spec: spec})
			if err != nil {
				return err
			}
			allDone = false
		} else if err != nil {
			return err
		}
		if !owns(*o, b.Metadata) {
			return fmt.Errorf("foreign backup collision")
		}
		if b.Status.Phase == model.BackupFailed {
			return fmt.Errorf("backup %s failed: %s", backupName, b.Status.Message)
		}
		if b.Status.Phase != model.BackupSucceeded {
			allDone = false
		}
	}
	if !allDone {
		e.status(o, "Running", "waiting for per-Machine quiesced backups")
		return nil
	}
	e.status(o, "Succeeded", "all backups succeeded; quiesce is per Machine, not a distributed database transaction")
	if s.KeepLast > 0 {
		backups, err := e.Kube.ListMachineBackupsNamespace(ctx, o.Metadata.Namespace)
		if err != nil {
			return err
		}
		for _, machine := range s.Machines {
			var owned []model.MachineBackup
			for _, b := range backups {
				if owns(*o, b.Metadata) && b.Spec.MachineName == machine && b.Status.Phase == model.BackupSucceeded && b.Metadata.DeletionTimestamp == nil {
					owned = append(owned, b)
				}
			}
			sort.Slice(owned, func(i, j int) bool {
				return owned[i].Metadata.CreationTimestamp.After(owned[j].Metadata.CreationTimestamp)
			})
			for i := s.KeepLast; i < len(owned); i++ {
				if err := e.Kube.DeleteMachineBackup(ctx, o.Metadata.Namespace, owned[i].Metadata.Name); err != nil {
					return err
				}
			}
		}
	}
	if s.IntervalSeconds > 0 && now.Sub(*o.Status.LastActionTime) >= time.Duration(s.IntervalSeconds)*time.Second {
		o.Status.LastActionTime = &now
		e.status(o, "Pending", "next backup batch reserved")
	}
	return nil
}

func (e *Engine) recovery(ctx context.Context, o *model.FleetResource) error {
	s, _ := decode[RecoverySpec](o.Spec)
	ns := o.Metadata.Namespace
	now := e.now()
	if !s.Start {
		e.status(o, "Paused", "set spec.start=true to execute the recovery plan")
		return nil
	}
	if o.Status.Phase == "Succeeded" {
		return nil
	}
	if o.Status.RecoveryStartedAt == nil {
		o.Status.RecoveryStartedAt = &now
		e.status(o, "Pending", "recovery execution reserved")
		return nil
	}
	for _, step := range s.Steps {
		if contains(o.Status.Completed, step.MachineName) {
			continue
		}
		ready := true
		for _, dep := range step.DependsOn {
			if !contains(o.Status.Completed, dep) {
				ready = false
			}
		}
		if !ready {
			continue
		}
		b, err := e.Kube.GetMachineBackup(ctx, ns, step.BackupName)
		if err != nil {
			return err
		}
		if b.Spec.MachineName != step.MachineName {
			return fmt.Errorf("backup %s belongs to another Machine", step.BackupName)
		}
		if b.Status.Phase != model.BackupSucceeded || b.Status.CompletionTime == nil {
			return fmt.Errorf("backup %s is not complete", step.BackupName)
		}
		age := now.Sub(*b.Status.CompletionTime).Seconds()
		if age < 0 || age > float64(s.MaxDataAgeSeconds) {
			return fmt.Errorf("backup exceeds the allowed data age")
		}
		if age > o.Status.DataAgeSeconds {
			o.Status.DataAgeSeconds = age
		}
		source, err := e.Kube.GetMachine(ctx, ns, step.MachineName)
		if err != nil {
			return err
		}
		target := source
		if s.Mode == "TestRestore" {
			// Image-owned disk restore supports another halted runtime on the same host.
			// Atlas PVC test cutover needs a separate disk attachment workflow.
			if !contains(e.IsolatedBridges, s.TestBridge) {
				return fmt.Errorf("TestRestore requires an administrator-allowlisted isolated bridge")
			}
			if len(b.Status.Volumes) > 0 || b.Status.Disk == nil {
				return fmt.Errorf("TestRestore currently requires an image-owned disk backup")
			}
			name := childName(*o, "test/"+step.MachineName)
			target, err = e.Kube.GetMachine(ctx, ns, name)
			if kube.IsNotFound(err) {
				target = source
				target.TypeMeta = model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine}
				target.Metadata = childMeta(*o, name)
				target.Status = model.MachineStatus{}
				target.Spec.NodeName = b.Status.NodeName
				target.Spec.PowerState = "Running"
				target.Spec.Network = model.NetworkSpec{Mode: "tap", Bridge: s.TestBridge, NetNS: true, DataplaneMode: "ebpf", DataplaneRequired: true}
				target.Spec.TTLSeconds = 3600
				target.Spec.CloudInit = model.CloudInitSpec{}
				target.Spec.ServiceFabric = model.ServiceFabricSpec{}
				target.Spec.DeviceClaims = nil
				target.Spec.Volumes = nil
				target.Spec.Disks = nil
				// Do not clone credentials, forward listeners or passthrough devices.
				if _, err = e.Kube.CreateMachine(ctx, ns, target); err != nil {
					return err
				}
				e.status(o, "PreparingTest", "creating an independent test runtime")
				return nil
			}
			if err != nil {
				return err
			}
			if !owns(*o, target.Metadata) {
				return fmt.Errorf("foreign test Machine collision")
			}
		}
		restoreName := childName(*o, "restore/"+step.MachineName)
		restore, err := e.Kube.GetMachineBackupRestore(ctx, ns, restoreName)
		if kube.IsNotFound(err) {
			if b.Status.Disk != nil {
				if target.Status.RuntimeID == "" {
					e.status(o, "Preparing", "waiting for runtime creation")
					return nil
				}
				if target.DesiredPowerState() != "Halted" {
					if err := e.disrupt(target); err != nil {
						return err
					}
					if err := e.Kube.PatchMachine(ctx, ns, target.Metadata.Name, map[string]any{"metadata": map[string]any{"resourceVersion": target.Metadata.ResourceVersion}, "spec": map[string]string{"powerState": "Halted"}}); err != nil {
						return err
					}
					e.status(o, "Halting", "waiting for target to halt")
					return nil
				}
			}
			_, err = e.Kube.CreateMachineBackupRestore(ctx, ns, model.MachineBackupRestore{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupRestore}, Metadata: childMeta(*o, restoreName), Spec: model.MachineBackupRestoreSpec{BackupName: step.BackupName, MachineName: target.Metadata.Name}})
			if err != nil {
				return err
			}
			e.status(o, "Restoring", "restore submitted")
			return nil
		}
		if err != nil {
			return err
		}
		if !owns(*o, restore.Metadata) {
			return fmt.Errorf("foreign restore collision")
		}
		if restore.Status.Phase == model.BackupFailed {
			return fmt.Errorf("restore failed: %s", restore.Status.Message)
		}
		if restore.Status.Phase != model.BackupSucceeded {
			e.status(o, "Restoring", "waiting for restore completion")
			return nil
		}
		if len(restore.Status.Volumes) > 0 {
			return fmt.Errorf("restored Atlas volumes; the operator must attach returned PVCs and verify application before completing recovery")
		}
		if target.DesiredPowerState() != "Running" {
			if err := e.Kube.PatchMachine(ctx, ns, target.Metadata.Name, map[string]any{"metadata": map[string]any{"resourceVersion": target.Metadata.ResourceVersion}, "spec": map[string]string{"powerState": "Running"}}); err != nil {
				return err
			}
			e.status(o, "Starting", "starting restored Machine")
			return nil
		}
		if target.Status.Phase != "Running" {
			e.status(o, "Starting", "waiting for restored Machine Running")
			return nil
		}
		// Completion is hypervisor readiness, not an application health assertion.
		o.Status.Completed = append(o.Status.Completed, step.MachineName)
		e.status(o, "Running", "restored "+step.MachineName)
		return nil
	}
	if len(o.Status.Completed) == len(s.Steps) {
		o.Status.RecoverySeconds = now.Sub(*o.Status.RecoveryStartedAt).Seconds()
		e.status(o, "Succeeded", "all restore targets Running; application validation is a separate check")
	}
	return nil
}

func (e *Engine) importPlan(ctx context.Context, o *model.FleetResource) error {
	s, _ := decode[ImportSpec](o.Spec)
	ns := o.Metadata.Namespace
	active := 0
	complete := 0
	pending := make([]ImportEntry, 0)
	for _, entry := range s.Entries {
		m, err := e.Kube.GetMachine(ctx, ns, entry.Name)
		if kube.IsNotFound(err) {
			pending = append(pending, entry)
			continue
		}
		if err != nil {
			return err
		}
		if !owns(*o, m.Metadata) {
			return fmt.Errorf("a Machine named %s already exists outside this import plan", entry.Name)
		}
		if m.Status.Phase == "Failed" {
			return fmt.Errorf("import of Machine %s failed: %s", entry.Name, m.Status.Message)
		}
		if m.Status.Phase == s.PowerState {
			complete++
		} else {
			active++
		}
	}
	for _, entry := range pending {
		if active >= s.MaxConcurrent {
			break
		}
		spec := model.MachineSpec{Image: entry.Image, Resources: entry.Resources, Runtime: model.RuntimeSpec{Backend: entry.Backend}, PowerState: s.PowerState}
		if spec.Runtime.Backend == "" {
			spec.Runtime.Backend = "qemu"
		}
		_, err := e.Kube.CreateMachine(ctx, ns, model.Machine{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine}, Metadata: childMeta(*o, entry.Name), Spec: spec})
		if err != nil {
			return err
		}
		active++
	}
	e.status(o, "Running", fmt.Sprintf("%d/%d imports reached requested power state", complete, len(s.Entries)))
	if complete == len(s.Entries) {
		e.status(o, "Succeeded", "all Machines reached requested power state; Stopped entries are staged, not imported until started")
	}
	return nil
}
