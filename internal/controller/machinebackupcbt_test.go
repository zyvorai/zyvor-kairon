// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func cbtNodes(names ...string) map[string]model.Node {
	out := map[string]model.Node{}
	for _, n := range names {
		var node model.Node
		node.Metadata.Name = n
		node.Metadata.Labels = map[string]string{model.CBTNodeLabel: "true"}
		out[n] = node
	}
	return out
}

func s3Target() *model.BackupTarget {
	return &model.BackupTarget{S3: &model.S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "vm", Prefix: "kairon", CredentialsSecretRef: "s3"}}
}

func cbtBackup(name, mode, parent string) *model.MachineBackup {
	return &model.MachineBackup{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", UID: name + "-0000-uid", CreationTimestamp: time.Now().Add(-time.Minute)},
		Spec:     model.MachineBackupSpec{MachineName: "web", Target: s3Target(), Mode: mode, Parent: parent},
	}
}

func succeeded(b *model.MachineBackup, seq int64, chain, base string, at time.Time) *model.MachineBackup {
	b.Status.Phase = model.BackupSucceeded
	b.Status.Kind = b.BackupMode()
	b.Status.PointInTime = &at
	b.Status.ManifestKey = "kairon/prod/web/" + chain + "/" + b.Metadata.Name + "/manifest.json"
	b.Status.Chain = &model.BackupChainStatus{ID: chain, Sequence: seq, BackupID: b.Metadata.Name, Base: base, Parent: b.Spec.Parent}
	return b
}

func TestCBTFullBackupStartsChainAndPromotesResult(t *testing.T) {
	b := cbtBackup("f1", "", "")
	b.Spec.ImmutableFor = "30d"
	fk := &backupKube{backups: map[string]*model.MachineBackup{"f1": b}, nodes: cbtNodes("n1")}
	c := newBackupTestController(t, fk, nil)
	machines := backupMachines(imageMachine("web", "n1"))
	ctx := context.Background()
	if err := c.reconcileMachineBackups(ctx, machines); err != nil {
		t.Fatal(err)
	}
	st := fk.backups["f1"].Status
	if st.Phase != model.BackupRunning || st.Kind != model.BackupModeFull || st.Chain == nil || st.Chain.Sequence != 1 ||
		!strings.HasPrefix(st.Chain.ID, "c-prod-f1-") || st.Chain.Base != "f1" || st.Disk == nil || st.Disk.Phase != model.BackupPending {
		t.Fatalf("status = %+v chain=%+v", st, st.Chain)
	}
	if st.RetentionUntil == nil || st.RetentionUntil.Before(time.Now().Add(29*24*time.Hour)) || st.PointInTime == nil {
		t.Fatalf("retentionUntil/pointInTime = %v %v", st.RetentionUntil, st.PointInTime)
	}
	if !model.HasFinalizerList(fk.backups["f1"].Metadata.Finalizers, model.FinalizerCBTBackup) {
		t.Fatalf("finalizers = %v", fk.backups["f1"].Metadata.Finalizers)
	}

	fk.backups["f1"].Status.Disk = &model.DiskBackupStatus{Phase: model.BackupSucceeded, JobID: "j1", ManifestKey: "k/manifest.json", StoredBytes: 10, ChangedBytes: 64, Objects: []string{"k/root.qcow2"}}
	if err := c.reconcileMachineBackups(ctx, machines); err != nil {
		t.Fatal(err)
	}
	st = fk.backups["f1"].Status
	if st.Phase != model.BackupSucceeded || st.ManifestKey != "k/manifest.json" || st.StoredBytes != 10 || st.ChangedBytes != 64 || len(st.Objects) != 1 {
		t.Fatalf("promoted status = %+v", st)
	}
}

func TestCBTBackupNeedsCapableNodeAndImageBoot(t *testing.T) {
	fk := &backupKube{backups: map[string]*model.MachineBackup{"f1": cbtBackup("f1", "", ""), "f2": cbtBackup("f2", "", "")}, nodes: map[string]model.Node{"n1": {}}}
	fk.backups["f2"].Spec.MachineName = "db"
	c := newBackupTestController(t, fk, nil)
	db := imageMachine("db", "n1")
	db.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: "db-root"}}
	if err := c.reconcileMachineBackups(context.Background(), backupMachines(imageMachine("web", "n1"), db)); err != nil {
		t.Fatal(err)
	}
	if st := fk.backups["f1"].Status; st.Phase != model.BackupFailed || !strings.Contains(st.Message, model.CBTNodeLabel) {
		t.Fatalf("f1 = %+v", st)
	}
	if st := fk.backups["f2"].Status; st.Phase != model.BackupFailed || !strings.Contains(st.Message, "spec.volumes") {
		t.Fatalf("f2 = %+v", st)
	}
}

func TestCBTIncrementalAndSyntheticFullFollowTheirParent(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	fk := &backupKube{
		backups: map[string]*model.MachineBackup{
			"f1": succeeded(cbtBackup("f1", "", ""), 1, "c1", "f1", t0),
			"i1": succeeded(cbtBackup("i1", model.BackupModeIncremental, "f1"), 2, "c1", "f1", t0.Add(time.Hour)),
			"s0": succeeded(cbtBackup("s0", model.BackupModeSyntheticFull, "f1"), 1, "c1", "s0", t0),
			"i2": cbtBackup("i2", model.BackupModeIncremental, "i1"),
			"s1": cbtBackup("s1", model.BackupModeSyntheticFull, "i1"),
			"bad": func() *model.MachineBackup {
				b := cbtBackup("bad", model.BackupModeIncremental, "f1")
				b.Spec.Target.S3.Bucket = "elsewhere"
				return b
			}(),
		},
		nodes: cbtNodes("n1"),
	}
	c := newBackupTestController(t, fk, nil)
	if err := c.reconcileMachineBackups(context.Background(), backupMachines(imageMachine("web", "n1"))); err != nil {
		t.Fatal(err)
	}
	i2 := fk.backups["i2"].Status
	if i2.Phase != model.BackupRunning || i2.Chain == nil || i2.Chain.ID != "c1" || i2.Chain.Sequence != 3 || i2.Chain.Parent != "i1" ||
		i2.Chain.ParentManifestKey != fk.backups["i1"].Status.ManifestKey || i2.Chain.Base != "f1" {
		t.Fatalf("i2 = %+v chain=%+v", i2, i2.Chain)
	}
	s1 := fk.backups["s1"].Status
	if s1.Phase != model.BackupRunning || s1.Chain == nil || s1.Chain.Sequence != 2 || s1.Chain.Base != "s1" ||
		!s1.PointInTime.Equal(*fk.backups["i1"].Status.PointInTime) || len(s1.Disk.SyntheticBases) != 1 {
		t.Fatalf("s1 = %+v chain=%+v disk=%+v", s1, s1.Chain, s1.Disk)
	}
	if st := fk.backups["bad"].Status; st.Phase != model.BackupFailed || !strings.Contains(st.Message, "different bucket") {
		t.Fatalf("bad = %+v", st)
	}
}

func TestCBTDeletionKeepsFinalizerOnceAJobStarted(t *testing.T) {
	now := time.Now()
	b := cbtBackup("f1", "", "")
	b.Metadata.DeletionTimestamp = &now
	b.Metadata.Finalizers = []string{model.FinalizerCBTBackup}
	b.Status = model.MachineBackupStatus{Phase: model.BackupFailed, Disk: &model.DiskBackupStatus{Phase: model.BackupFailed}}
	fk := &backupKube{backups: map[string]*model.MachineBackup{"f1": b}}
	c := newBackupTestController(t, fk, nil)
	if err := c.reconcileMachineBackups(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(b.Metadata.Finalizers) != 0 {
		t.Fatalf("finalizers = %v", b.Metadata.Finalizers)
	}
	b.Metadata.Finalizers = []string{model.FinalizerCBTBackup}
	b.Status.Disk.JobID = "j1"
	if err := c.reconcileMachineBackups(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(b.Metadata.Finalizers) != 1 {
		t.Fatalf("controller dropped kairon-node's finalizer: %v", b.Metadata.Finalizers)
	}
}

func TestCBTRestoreRunsOnAnyCapableNode(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	fk := &backupKube{
		backups: map[string]*model.MachineBackup{
			"f1": succeeded(cbtBackup("f1", "", ""), 1, "c1", "f1", t0),
			"s1": succeeded(cbtBackup("s1", model.BackupModeSyntheticFull, "f1"), 1, "c1", "s1", t0),
		},
		restores: map[string]*model.MachineBackupRestore{
			"r1": {Metadata: model.ObjectMeta{Name: "r1", Namespace: "prod"}, Spec: model.MachineBackupRestoreSpec{BackupName: "f1", MachineName: "other"}},
		},
		nodes: cbtNodes("n2"),
	}
	fk.backups["f1"].Status.NodeName = "n1"
	c := newBackupTestController(t, fk, nil)
	if err := c.reconcileMachineBackups(context.Background(), backupMachines(imageMachine("web", "n1"), imageMachine("other", "n2"))); err != nil {
		t.Fatal(err)
	}
	st := fk.restores["r1"].Status
	if st.Phase != model.BackupRunning || st.NodeName != "n2" || st.Disk == nil || st.Disk.ManifestKey != fk.backups["f1"].Status.ManifestKey ||
		len(st.Disk.SyntheticBases) != 1 || st.Disk.SyntheticBases[0] != fk.backups["s1"].Status.ManifestKey {
		t.Fatalf("restore status = %+v disk=%+v", st, st.Disk)
	}
}

func TestBackupPolicyCreatesFullThenIncrementalAndRecordsStatus(t *testing.T) {
	policy := &model.MachineBackupPolicy{
		Metadata: model.ObjectMeta{Name: "nightly", Namespace: "prod"},
		Spec: model.MachineBackupPolicySpec{
			Selector:         map[string]string{"tier": "web"},
			IncrementalEvery: "1h",
			Target:           *s3Target(),
			Retention:        model.BackupRetention{Hourly: 24},
		},
	}
	fk := &backupKube{backups: map[string]*model.MachineBackup{}, nodes: cbtNodes("n1"), policies: map[string]*model.MachineBackupPolicy{"nightly": policy}}
	c := newBackupTestController(t, fk, nil)
	web := imageMachine("web", "n1")
	web.Metadata.Labels = map[string]string{"tier": "web"}
	pvc := imageMachine("db", "n1")
	pvc.Metadata.Labels = map[string]string{"tier": "web"}
	pvc.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: "db-root"}}
	machines := []model.Machine{web, pvc, imageMachine("unrelated", "n1")}
	ctx := context.Background()

	if err := c.reconcileMachineBackupPolicies(ctx, machines); err != nil {
		t.Fatal(err)
	}
	if len(fk.backups) != 1 {
		t.Fatalf("backups = %v", fk.backups)
	}
	var full *model.MachineBackup
	for _, b := range fk.backups {
		full = b
	}
	if full.BackupMode() != model.BackupModeFull || full.Spec.MachineName != "web" || full.Spec.PolicyName != "nightly" ||
		full.Metadata.Labels[model.BackupPolicyLabel] != "nightly" || full.Metadata.Labels[model.BackupMachineLabel] != "web" {
		t.Fatalf("full = %+v", full)
	}
	ms := policy.Status.Machines
	if len(ms) != 2 || ms[0].MachineName != "web" || ms[1].MachineName != "db" || !strings.Contains(ms[1].Skipped, "spec.volumes") {
		t.Fatalf("status = %+v", ms)
	}

	// In flight: nothing new.
	if err := c.reconcileMachineBackupPolicies(ctx, machines); err != nil {
		t.Fatal(err)
	}
	if len(fk.backups) != 1 {
		t.Fatalf("created while one was in flight: %d", len(fk.backups))
	}

	full.Metadata.CreationTimestamp = time.Now().Add(-2 * time.Hour)
	succeeded(full, 1, "c1", full.Metadata.Name, full.Metadata.CreationTimestamp)
	if err := c.reconcileMachineBackupPolicies(ctx, machines); err != nil {
		t.Fatal(err)
	}
	var incr *model.MachineBackup
	for _, b := range fk.backups {
		if b.BackupMode() == model.BackupModeIncremental {
			incr = b
		}
	}
	if incr == nil || incr.Spec.Parent != full.Metadata.Name {
		t.Fatalf("incremental = %+v", incr)
	}
	if ms := policy.Status.Machines; ms[0].ChainID != "c1" || ms[0].LastFull == nil {
		t.Fatalf("status = %+v", ms[0])
	}
}

func TestBackupPolicyRetentionDeletesReportsHeldAndSynthesizes(t *testing.T) {
	policy := &model.MachineBackupPolicy{
		Metadata: model.ObjectMeta{Name: "p", Namespace: "prod"},
		Spec: model.MachineBackupPolicySpec{
			Selector:         map[string]string{"tier": "web"},
			IncrementalEvery: "1h",
			FullEvery:        "1000h",
			Target:           *s3Target(),
			Retention:        model.BackupRetention{Hourly: 2},
			Suspend:          true,
		},
	}
	t0 := time.Now().Add(-10 * time.Hour).Truncate(time.Hour)
	mk := func(name, mode, parent string, seq int64, at time.Time) *model.MachineBackup {
		b := succeeded(cbtBackup(name, mode, parent), seq, "c1", "f1", at)
		b.Metadata.Labels = map[string]string{model.BackupPolicyLabel: "p"}
		b.Spec.PolicyName = "p"
		return b
	}
	held := time.Now().Add(time.Hour)
	fk := &backupKube{
		backups: map[string]*model.MachineBackup{
			"f1": mk("f1", "", "", 1, t0),
			"i1": mk("i1", model.BackupModeIncremental, "f1", 2, t0.Add(time.Hour)),
			"i2": mk("i2", model.BackupModeIncremental, "i1", 3, t0.Add(2*time.Hour)),
			"i3": mk("i3", model.BackupModeIncremental, "i2", 4, t0.Add(3*time.Hour)),
		},
		nodes:    cbtNodes("n1"),
		policies: map[string]*model.MachineBackupPolicy{"p": policy},
	}
	fk.backups["i1"].Status.RetentionUntil = &held
	c := newBackupTestController(t, fk, nil)
	web := imageMachine("web", "n1")
	web.Metadata.Labels = map[string]string{"tier": "web"}
	if err := c.reconcileMachineBackupPolicies(context.Background(), []model.Machine{web}); err != nil {
		t.Fatal(err)
	}
	// i2 and i3 are kept; the whole chain back to f1 is still their path,
	// so nothing is deleted yet. Suspended: no synthetic full is requested.
	for _, n := range []string{"f1", "i1", "i2", "i3"} {
		if fk.backups[n] == nil {
			t.Fatalf("%s deleted", n)
		}
	}
	policy.Spec.Suspend = false
	if err := c.reconcileMachineBackupPolicies(context.Background(), []model.Machine{web}); err != nil {
		t.Fatal(err)
	}
	var synth *model.MachineBackup
	for _, b := range fk.backups {
		if b.BackupMode() == model.BackupModeSyntheticFull {
			synth = b
		}
	}
	if synth == nil || synth.Spec.Parent != "i2" {
		t.Fatalf("synthetic = %+v", synth)
	}

	// Once the synthetic full of i2 succeeds it stands in for i2 on i3's
	// path, so f1, i1 and i2 are unneeded; i1 is under Object Lock.
	succeeded(synth, 3, "c1", synth.Metadata.Name, t0.Add(2*time.Hour))
	if err := c.reconcileMachineBackupPolicies(context.Background(), []model.Machine{web}); err != nil {
		t.Fatal(err)
	}
	if fk.backups["f1"] != nil || fk.backups["i2"] != nil || fk.backups["i1"] == nil || fk.backups["i3"] == nil {
		t.Fatalf("after synthesis: %v", fk.backups)
	}
	if h := policy.Status.Machines[0].RetentionHeld; len(h) != 1 || h[0] != "i1" {
		t.Fatalf("held = %v", h)
	}
}
