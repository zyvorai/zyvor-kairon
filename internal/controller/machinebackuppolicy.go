// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileMachineBackupPolicies runs every MachineBackupPolicy: for each
// selected Machine it creates the next full or incremental S3 MachineBackup
// when one is due (never while one is in flight), applies GFS retention --
// deleting backups no kept point needs, reporting those Object Lock still
// holds, and requesting a synthetic full where a kept point's chain still
// reaches back into a full retention wants gone -- and records per-Machine
// status. Tolerant of the CRD not being installed.
func (c *Controller) reconcileMachineBackupPolicies(ctx context.Context, machines []model.Machine) error {
	policies, err := c.Kube.ListMachineBackupPolicies(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	if len(policies) == 0 {
		return nil
	}
	backups, err := c.Kube.ListMachineBackups(ctx)
	if err != nil && !kube.IsNotFound(err) {
		return err
	}
	now := time.Now().UTC()
	nodeCBT := map[string]bool{}
	cbtNode := func(name string) bool {
		ok, seen := nodeCBT[name]
		if !seen {
			ok = c.cbtNodeReady(ctx, name) == nil
			nodeCBT[name] = ok
		}
		return ok
	}
	for _, p := range policies {
		status := c.runBackupPolicy(ctx, p, machines, backups, cbtNode, now)
		if reflect.DeepEqual(status, p.Status) {
			continue
		}
		if err := c.Kube.PatchMachineBackupPolicyStatus(ctx, p.Namespace(), p.Metadata.Name, status); err != nil {
			c.Log.Error("machine backup policy status patch failed", "namespace", p.Namespace(), "policy", p.Metadata.Name, "error", err)
		}
	}
	return nil
}

func (c *Controller) runBackupPolicy(ctx context.Context, p model.MachineBackupPolicy, machines []model.Machine, all []model.MachineBackup, cbtNode func(string) bool, now time.Time) model.MachineBackupPolicyStatus {
	var status model.MachineBackupPolicyStatus
	specErr := p.Spec.Validate()
	for _, m := range machines {
		if m.Namespace() != p.Namespace() || !model.LabelsMatch(m.Metadata.Labels, p.Spec.Selector) {
			continue
		}
		st := model.PolicyMachineStatus{MachineName: m.Metadata.Name}
		var mine []model.MachineBackup
		for _, b := range all {
			if b.Namespace() == p.Namespace() && b.Metadata.Labels[model.BackupPolicyLabel] == p.Metadata.Name && b.Spec.MachineName == m.Metadata.Name {
				mine = append(mine, b)
			}
		}
		summarizeChain(&st, mine)
		if c.Metrics != nil {
			length, last := chainGauge(mine, st.ChainID)
			c.Metrics.ObserveBackupChain(p.Namespace(), p.Metadata.Name, m.Metadata.Name, length, last)
		}
		switch {
		case specErr != nil:
			st.LastError = "invalid spec: " + specErr.Error()
		case len(m.Spec.Volumes) > 0:
			st.Skipped = "boots from spec.volumes; S3 backups need FluxVM-owned qcow2 disks"
		case m.Spec.NodeName == "":
			st.Skipped = "not scheduled to a node"
		case !cbtNode(m.Spec.NodeName):
			st.Skipped = fmt.Sprintf("node %s has no FluxVM with S3 incremental backups", m.Spec.NodeName)
		default:
			c.policyBackup(ctx, p, m, mine, &st, now)
		}
		if specErr == nil {
			c.policyRetention(ctx, p, m, mine, &st, now)
		}
		status.Machines = append(status.Machines, st)
	}
	return status
}

func (c *Controller) policyBackup(ctx context.Context, p model.MachineBackupPolicy, m model.Machine, mine []model.MachineBackup, st *model.PolicyMachineStatus, now time.Time) {
	run, err := model.PlanPolicyRun(p.Spec, mine, now)
	if err != nil {
		st.LastError = err.Error()
		return
	}
	if !run.NextRun.IsZero() {
		next := run.NextRun
		st.NextRun = &next
	}
	if run.Mode == "" {
		return
	}
	if err := c.createPolicyBackup(ctx, p, m, run.Mode, run.Parent, now); err != nil {
		st.LastError = err.Error()
	}
}

func (c *Controller) policyRetention(ctx context.Context, p model.MachineBackupPolicy, m model.Machine, mine []model.MachineBackup, st *model.PolicyMachineStatus, now time.Time) {
	plan := model.PlanRetention(mine, p.Spec.Retention, now)
	st.RetentionHeld = plan.Held
	for _, name := range plan.Delete {
		if err := c.Kube.DeleteMachineBackup(ctx, p.Namespace(), name); err != nil && !kube.IsNotFound(err) {
			c.Log.Error("backup policy retention delete failed", "namespace", p.Namespace(), "policy", p.Metadata.Name, "backup", name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("backuppolicy")
			}
			st.LastError = fmt.Sprintf("delete %s: %v", name, err)
		}
	}
	if plan.Synthesize == "" || p.Spec.Suspend || synthesizing(mine) {
		return
	}
	if err := c.createPolicyBackup(ctx, p, m, model.BackupModeSyntheticFull, plan.Synthesize, now); err != nil {
		st.LastError = err.Error()
	}
}

func synthesizing(backups []model.MachineBackup) bool {
	for _, b := range backups {
		if b.BackupMode() == model.BackupModeSyntheticFull && b.Metadata.DeletionTimestamp == nil &&
			b.Status.Phase != model.BackupSucceeded && b.Status.Phase != model.BackupFailed {
			return true
		}
	}
	return false
}

func (c *Controller) createPolicyBackup(ctx context.Context, p model.MachineBackupPolicy, m model.Machine, mode, parent string, now time.Time) error {
	short := map[string]string{model.BackupModeFull: "full", model.BackupModeIncremental: "incr", model.BackupModeSyntheticFull: "synth"}[mode]
	target := p.Spec.Target
	b := model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s-%d", p.Metadata.Name, m.Metadata.Name, short, now.Unix()),
			Namespace: p.Namespace(),
			Labels:    map[string]string{model.BackupPolicyLabel: p.Metadata.Name, model.BackupMachineLabel: m.Metadata.Name},
		},
		Spec: model.MachineBackupSpec{
			MachineName:  m.Metadata.Name,
			Quiesce:      p.Spec.Quiesce,
			Target:       &target,
			Mode:         mode,
			Parent:       parent,
			PolicyName:   p.Metadata.Name,
			Compression:  p.Spec.Compression,
			Encryption:   p.Spec.Encryption,
			ImmutableFor: p.Spec.ImmutableFor,
		},
	}
	if _, err := c.Kube.CreateMachineBackup(ctx, p.Namespace(), b); err != nil {
		if c.Metrics != nil {
			c.Metrics.ObserveReconcileItemError("backuppolicy")
		}
		return fmt.Errorf("create %s backup: %w", mode, err)
	}
	c.Log.Info("backup policy created backup", "namespace", p.Namespace(), "policy", p.Metadata.Name, "machine", m.Metadata.Name, "backup", b.Metadata.Name, "mode", mode)
	return nil
}

// summarizeChain fills the chain fields of a policy Machine's status from
// its successful backups.
func summarizeChain(st *model.PolicyMachineStatus, backups []model.MachineBackup) {
	var newest time.Time
	for _, b := range backups {
		if b.Status.Phase != model.BackupSucceeded || b.Status.PointInTime == nil || b.Metadata.DeletionTimestamp != nil {
			continue
		}
		t := *b.Status.PointInTime
		switch b.Status.Kind {
		case model.BackupModeFull:
			if st.LastFull == nil || t.After(*st.LastFull) {
				st.LastFull = &t
			}
		case model.BackupModeIncremental:
			if st.LastIncremental == nil || t.After(*st.LastIncremental) {
				st.LastIncremental = &t
			}
		default:
			continue
		}
		if b.Status.Chain != nil && !t.Before(newest) {
			newest = t
			st.ChainID = b.Status.Chain.ID
		}
	}
}

// chainGauge counts the successful fulls and incrementals of a chain and
// returns its newest point.
func chainGauge(backups []model.MachineBackup, chainID string) (int, time.Time) {
	n, last := 0, time.Time{}
	for _, b := range backups {
		if chainID == "" || b.Status.Phase != model.BackupSucceeded || b.Status.Chain == nil || b.Status.Chain.ID != chainID ||
			b.BackupMode() == model.BackupModeSyntheticFull || b.Metadata.DeletionTimestamp != nil {
			continue
		}
		n++
		if b.Status.PointInTime != nil && b.Status.PointInTime.After(last) {
			last = *b.Status.PointInTime
		}
	}
	return n, last
}
