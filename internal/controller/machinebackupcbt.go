// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// startCBTBackup places a MachineBackup with spec.target in its chain and
// hands it to kairon-node on the Machine's node. FluxVM tracks changed
// blocks only for disks it owns, so the Machine must boot from an image.
func (c *Controller) startCBTBackup(ctx context.Context, b model.MachineBackup, machine model.Machine, status map[string]any) error {
	if err := model.ValidateCBTBackup(b); err != nil {
		return err
	}
	if len(machine.Spec.Volumes) > 0 {
		return fmt.Errorf("machine %s boots from spec.volumes: S3 backups need FluxVM-owned qcow2 disks (use MachineSnapshot for volumes)", machine.Metadata.Name)
	}
	if machine.Spec.NodeName == "" {
		return fmt.Errorf("machine %s is not scheduled to a node yet", machine.Metadata.Name)
	}
	if err := c.cbtNodeReady(ctx, machine.Spec.NodeName); err != nil {
		return err
	}
	backups, err := c.Kube.ListMachineBackupsNamespace(ctx, b.Namespace())
	if err != nil {
		return fmt.Errorf("list backups: %w", err)
	}
	chain, point, err := cbtChain(b, backups, time.Now().UTC())
	if err != nil {
		return err
	}
	disk := model.DiskBackupStatus{Phase: model.BackupPending, Message: "waiting for kairon-node on " + machine.Spec.NodeName}
	if b.BackupMode() == model.BackupModeSyntheticFull {
		disk.SyntheticBases = syntheticBasesIn(backups, chain.ID)
	}
	if !model.HasFinalizerList(b.Metadata.Finalizers, model.FinalizerCBTBackup) {
		finals := append(append([]string{}, b.Metadata.Finalizers...), model.FinalizerCBTBackup)
		if err := c.Kube.PatchMachineBackup(ctx, b.Namespace(), b.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
			return fmt.Errorf("add backup finalizer: %w", err)
		}
	}
	status["nodeName"] = machine.Spec.NodeName
	status["disk"] = disk
	status["chain"] = chain
	status["kind"] = b.BackupMode()
	status["pointInTime"] = point
	if b.Spec.ImmutableFor != "" {
		d, _ := model.ParseBackupDuration(b.Spec.ImmutableFor)
		status["retentionUntil"] = time.Now().UTC().Add(d)
	}
	return nil
}

// cbtChain places b in its chain: a full starts one; an incremental
// follows its parent (spec.parent, or the newest successful backup of its
// policy); a synthetic full stands in for the point it consolidates.
func cbtChain(b model.MachineBackup, backups []model.MachineBackup, now time.Time) (model.BackupChainStatus, time.Time, error) {
	backupID := strings.TrimPrefix(model.FluxVMBackupName(b), "kairon-")
	if b.BackupMode() == model.BackupModeFull {
		return model.BackupChainStatus{ID: model.CBTChainID(b), Sequence: 1, BackupID: backupID, Base: b.Metadata.Name}, now, nil
	}
	parentName := b.Spec.Parent
	if parentName == "" {
		parentName = newestPolicyBackup(b, backups)
		if parentName == "" {
			return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("policy %s has no successful backup of machine %s to build on", b.Spec.PolicyName, b.Spec.MachineName)
		}
	}
	var parent *model.MachineBackup
	for i := range backups {
		if backups[i].Metadata.Name == parentName {
			parent = &backups[i]
		}
	}
	switch {
	case parent == nil:
		return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("parent backup %s not found", parentName)
	case parent.Metadata.DeletionTimestamp != nil:
		return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("parent backup %s is being deleted", parentName)
	case parent.Status.Phase != model.BackupSucceeded || parent.Status.Chain == nil || parent.Status.ManifestKey == "":
		return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("parent backup %s has not succeeded as an S3 backup", parentName)
	case parent.Spec.MachineName != b.Spec.MachineName:
		return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("parent backup %s is of machine %s", parentName, parent.Spec.MachineName)
	case !model.SameBackupTarget(*parent, b):
		return model.BackupChainStatus{}, time.Time{}, fmt.Errorf("parent backup %s went to a different bucket or prefix", parentName)
	}
	pc := parent.Status.Chain
	chain := model.BackupChainStatus{
		ID:                pc.ID,
		BackupID:          backupID,
		Parent:            parent.Metadata.Name,
		ParentBackupID:    pc.BackupID,
		ParentManifestKey: parent.Status.ManifestKey,
		Base:              pc.Base,
	}
	if b.BackupMode() == model.BackupModeSyntheticFull {
		chain.Sequence = pc.Sequence
		chain.Base = b.Metadata.Name
		point := parent.Metadata.CreationTimestamp
		if parent.Status.PointInTime != nil {
			point = *parent.Status.PointInTime
		}
		return chain, point, nil
	}
	chain.Sequence = pc.Sequence + 1
	return chain, now, nil
}

func newestPolicyBackup(b model.MachineBackup, backups []model.MachineBackup) string {
	var best *model.MachineBackup
	for i := range backups {
		x := &backups[i]
		if x.Spec.PolicyName != b.Spec.PolicyName || x.Spec.MachineName != b.Spec.MachineName ||
			x.Status.Phase != model.BackupSucceeded || x.Metadata.DeletionTimestamp != nil ||
			x.BackupMode() == model.BackupModeSyntheticFull || x.Status.PointInTime == nil {
			continue
		}
		if best == nil || x.Status.PointInTime.After(*best.Status.PointInTime) {
			best = x
		}
	}
	if best == nil {
		return ""
	}
	return best.Metadata.Name
}

// syntheticBasesIn lists the manifests of a chain's successful synthetic
// fulls: FluxVM reads one in place of the backups it consolidated, which
// retention may already have deleted.
func syntheticBasesIn(backups []model.MachineBackup, chainID string) []string {
	var out []string
	for _, x := range backups {
		if x.BackupMode() == model.BackupModeSyntheticFull && x.Status.Phase == model.BackupSucceeded &&
			x.Metadata.DeletionTimestamp == nil && x.Status.Chain != nil && x.Status.Chain.ID == chainID && x.Status.ManifestKey != "" {
			out = append(out, x.Status.ManifestKey)
		}
	}
	sort.Strings(out)
	return out
}

func (c *Controller) syntheticBases(ctx context.Context, b model.MachineBackup) ([]string, error) {
	if b.Status.Chain == nil {
		return nil, nil
	}
	backups, err := c.Kube.ListMachineBackupsNamespace(ctx, b.Namespace())
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	return syntheticBasesIn(backups, b.Status.Chain.ID), nil
}

// cbtNodeReady checks that kairon-node found a CBT-capable FluxVM there.
func (c *Controller) cbtNodeReady(ctx context.Context, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("machine is not scheduled to a node yet")
	}
	node, err := c.Kube.GetNode(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("get node %s: %w", nodeName, err)
	}
	if node.Metadata.Labels[model.CBTNodeLabel] != "true" {
		return fmt.Errorf("node %s has no FluxVM with S3 incremental backups (label %s=true)", nodeName, model.CBTNodeLabel)
	}
	return nil
}

// cbtWroteAnything reports whether kairon-node may have objects in S3 to
// delete for b.
func cbtWroteAnything(b model.MachineBackup) bool {
	if b.Status.ManifestKey != "" || len(b.Status.Objects) > 0 {
		return true
	}
	d := b.Status.Disk
	return d != nil && (d.JobID != "" || d.ManifestKey != "" || len(d.Objects) > 0)
}

// promoteCBTResult copies kairon-node's disk result to the backup's
// top-level status.
func promoteCBTResult(b model.MachineBackup, status map[string]any) {
	d := b.Status.Disk
	if d == nil {
		return
	}
	status["manifestKey"] = d.ManifestKey
	status["storedBytes"] = d.StoredBytes
	status["changedBytes"] = d.ChangedBytes
	status["objects"] = d.Objects
}
