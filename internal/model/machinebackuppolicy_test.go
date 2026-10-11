// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// cbt builds a backup at t0+at: mode full/incremental/synthetic-full,
// parent the chain parent (or the consolidated point for a synthetic).
func cbt(name, mode, parent, chain string, at time.Duration, phase string) MachineBackup {
	ts := t0.Add(at)
	b := MachineBackup{
		Metadata: ObjectMeta{Name: name, CreationTimestamp: ts},
		Spec:     MachineBackupSpec{Mode: mode, Parent: parent},
		Status: MachineBackupStatus{
			Phase:       phase,
			Kind:        mode,
			PointInTime: &ts,
			Chain:       &BackupChainStatus{ID: chain, Parent: parent},
		},
	}
	if mode == BackupModeSyntheticFull {
		b.Status.Chain.Parent = ""
	}
	return b
}

func policySpec() MachineBackupPolicySpec {
	return MachineBackupPolicySpec{
		Selector:         map[string]string{"app": "db"},
		IncrementalEvery: "1h",
		FullEvery:        "24h",
		Target:           BackupTarget{S3: &S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "b", CredentialsSecretRef: "s3"}},
	}
}

func TestPlanPolicyRunPicksTheMode(t *testing.T) {
	spec := policySpec()
	ok := BackupSucceeded
	fullRequired := cbt("i2", BackupModeIncremental, "i1", "c1", 2*time.Hour, BackupFailed)
	fullRequired.Status.Disk = &DiskBackupStatus{FullRequired: true}
	plainFail := cbt("i2", BackupModeIncremental, "i1", "c1", 2*time.Hour, BackupFailed)
	chain := []MachineBackup{
		cbt("f0", BackupModeFull, "", "c1", 0, ok),
		cbt("i1", BackupModeIncremental, "f0", "c1", time.Hour, ok),
	}
	for _, tc := range []struct {
		name    string
		backups []MachineBackup
		now     time.Duration
		mode    string
		parent  string
		waiting string
	}{
		{name: "no chain yet", now: 0, mode: BackupModeFull},
		{name: "not due", backups: chain, now: time.Hour + 30*time.Minute},
		{name: "incremental on newest", backups: chain, now: 2 * time.Hour, mode: BackupModeIncremental, parent: "i1"},
		{name: "fullEvery elapsed", backups: chain, now: 24 * time.Hour, mode: BackupModeFull},
		{name: "bitmap lost forces a full at once", backups: append(chain[:2:2], fullRequired), now: 2*time.Hour + time.Minute, mode: BackupModeFull},
		{name: "plain failure waits an interval", backups: append(chain[:2:2], plainFail), now: 2*time.Hour + time.Minute},
		{name: "plain failure then incremental", backups: append(chain[:2:2], plainFail), now: 3 * time.Hour, mode: BackupModeIncremental, parent: "i1"},
		{name: "in flight", backups: append(chain[:2:2], cbt("i2", BackupModeIncremental, "i1", "c1", 2*time.Hour, BackupRunning)), now: 5 * time.Hour, waiting: "i2"},
		{name: "synthesis in flight", backups: append(chain[:2:2], cbt("s1", BackupModeSyntheticFull, "i1", "c1", 2*time.Hour, "")), now: 5 * time.Hour, waiting: "s1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanPolicyRun(spec, tc.backups, t0.Add(tc.now))
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode != tc.mode || got.Parent != tc.parent || got.Waiting != tc.waiting {
				t.Fatalf("got %+v, want mode %q parent %q waiting %q", got, tc.mode, tc.parent, tc.waiting)
			}
		})
	}

	full := spec
	full.Mode = BackupModeFull
	if got, _ := PlanPolicyRun(full, chain, t0.Add(3*time.Hour)); got.Mode != BackupModeFull {
		t.Fatalf("full-only policy: %+v", got)
	}
	suspended := spec
	suspended.Suspend = true
	if got, _ := PlanPolicyRun(suspended, nil, t0); got.Mode != "" {
		t.Fatalf("suspended: %+v", got)
	}
}

func TestGFSKeepsNewestPerBucket(t *testing.T) {
	points := map[string]time.Time{}
	// Every 20 minutes for three days.
	for i := 0; i < 3*72; i++ {
		points[time.Duration(i).String()] = t0.Add(time.Duration(i) * 20 * time.Minute)
	}
	keep := GFSKeep(points, BackupRetention{Hourly: 3, Daily: 2})
	var kept []time.Time
	for n := range keep {
		kept = append(kept, points[n])
	}
	if len(keep) != 4 {
		t.Fatalf("kept %d: %v", len(keep), kept)
	}
	newest := t0.Add(215 * 20 * time.Minute)
	for _, want := range []time.Time{newest, newest.Add(-time.Hour), newest.Add(-2 * time.Hour), t0.Add(143 * 20 * time.Minute)} {
		found := false
		for _, k := range kept {
			found = found || k.Equal(want)
		}
		if !found {
			t.Fatalf("missing %v in %v", want, kept)
		}
	}
	if all := GFSKeep(points, BackupRetention{}); len(all) != len(points) {
		t.Fatalf("zero retention keeps everything, kept %d", len(all))
	}
}

func TestRetentionKeepsDependenciesAndAsksForASyntheticFull(t *testing.T) {
	ok := BackupSucceeded
	day := 24 * time.Hour
	backups := []MachineBackup{
		cbt("f0", BackupModeFull, "", "c1", 0, ok),
		cbt("i1", BackupModeIncremental, "f0", "c1", day, ok),
		cbt("i2", BackupModeIncremental, "i1", "c1", 2*day, ok),
		cbt("i3", BackupModeIncremental, "i2", "c1", 3*day, ok),
	}
	r := BackupRetention{Daily: 2}
	plan := PlanRetention(backups, r, t0.Add(4*day))
	// i3 and i2 are kept and need i1 and f0; nothing goes yet, and the
	// oldest kept point is consolidated to free the old base.
	if len(plan.Delete) != 0 || plan.Synthesize != "i2" {
		t.Fatalf("before synthesis: %+v", plan)
	}

	syn := cbt("s2", BackupModeSyntheticFull, "i2", "c1", 4*day, ok)
	syn.Status.PointInTime = backups[2].Status.PointInTime
	backups = append(backups, syn)
	plan = PlanRetention(backups, r, t0.Add(4*day))
	if !reflect.DeepEqual(plan.Delete, []string{"f0", "i1", "i2"}) || plan.Synthesize != "" {
		t.Fatalf("after synthesis: %+v", plan)
	}

	// Once i2 is gone the synthetic full stands for its point and stays.
	backups = []MachineBackup{backups[3], backups[4]}
	plan = PlanRetention(backups, r, t0.Add(4*day))
	if len(plan.Delete) != 0 || plan.Synthesize != "" {
		t.Fatalf("steady state: %+v", plan)
	}
}

func TestRetentionReportsObjectLockHolds(t *testing.T) {
	ok := BackupSucceeded
	old := cbt("f0", BackupModeFull, "", "c1", 0, ok)
	until := t0.Add(48 * time.Hour)
	old.Status.RetentionUntil = &until
	backups := []MachineBackup{old, cbt("f1", BackupModeFull, "", "c2", time.Hour, ok)}
	plan := PlanRetention(backups, BackupRetention{Hourly: 1}, t0.Add(2*time.Hour))
	if len(plan.Delete) != 0 || !reflect.DeepEqual(plan.Held, []string{"f0"}) {
		t.Fatalf("held: %+v", plan)
	}
	plan = PlanRetention(backups, BackupRetention{Hourly: 1}, t0.Add(49*time.Hour))
	if !reflect.DeepEqual(plan.Delete, []string{"f0"}) {
		t.Fatalf("expired: %+v", plan)
	}
}

func TestRetentionNeverDeletesWhatInFlightWorkReads(t *testing.T) {
	ok := BackupSucceeded
	backups := []MachineBackup{
		cbt("f0", BackupModeFull, "", "c1", 0, ok),
		cbt("i1", BackupModeIncremental, "f0", "c1", time.Hour, ok),
		cbt("f2", BackupModeFull, "", "c2", 2*time.Hour, ok),
		cbt("s1", BackupModeSyntheticFull, "i1", "c1", 3*time.Hour, BackupRunning),
	}
	plan := PlanRetention(backups, BackupRetention{Hourly: 1}, t0.Add(4*time.Hour))
	if len(plan.Delete) != 0 {
		t.Fatalf("deleted under a running synthesis: %+v", plan)
	}
}

func TestValidateCBTBackupAndPolicy(t *testing.T) {
	target := &BackupTarget{S3: &S3BackupTarget{Endpoint: "https://s3", Bucket: "b", CredentialsSecretRef: "c"}}
	good := MachineBackup{Spec: MachineBackupSpec{MachineName: "m", Target: target, Mode: BackupModeIncremental, Parent: "p", ImmutableFor: "30d"}}
	if err := ValidateCBTBackup(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*MachineBackup){
		"incremental without parent": func(b *MachineBackup) { b.Spec.Parent = "" },
		"bad mode":                   func(b *MachineBackup) { b.Spec.Mode = "differential" },
		"gzip":                       func(b *MachineBackup) { b.Spec.Compression = "gzip" },
		"bad immutability":           func(b *MachineBackup) { b.Spec.ImmutableFor = "forever" },
		"no key ref":                 func(b *MachineBackup) { b.Spec.Encryption = &BackupEncryption{} },
		"traversal prefix": func(b *MachineBackup) {
			b.Spec.Target = &BackupTarget{S3: &S3BackupTarget{Endpoint: "https://s3", Bucket: "b", CredentialsSecretRef: "c", Prefix: "a/../b"}}
		},
		"immutable without target": func(b *MachineBackup) { b.Spec.Target = nil },
	} {
		b := good
		mutate(&b)
		if err := ValidateCBTBackup(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := policySpec().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := policySpec()
	bad.IncrementalEvery = "1m"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "5m") {
		t.Fatalf("short interval: %v", err)
	}
	b := MachineBackup{Metadata: ObjectMeta{Name: "db-1", Namespace: "prod", UID: "abcdef0123"}, Spec: MachineBackupSpec{MachineName: "db", Target: target}}
	if CBTPrefix(b) != "prod/db" || CBTChainID(b) != "c-prod-db-1-abcdef01" {
		t.Fatalf("prefix %q chain %q", CBTPrefix(b), CBTChainID(b))
	}
	if d, _ := ParseBackupDuration("2d"); d != 48*time.Hour {
		t.Fatalf("2d = %v", d)
	}
}
