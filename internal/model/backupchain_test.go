// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"reflect"
	"testing"
	"time"
)

func TestBackupChainsGroupsAndIndents(t *testing.T) {
	ok := BackupSucceeded
	s3 := &BackupTarget{S3: &S3BackupTarget{Endpoint: "http://minio:9000", Bucket: "b", CredentialsSecretRef: "s3"}}
	mk := func(name, mode, parent, chain string, seq int64, at time.Duration, stored int64) MachineBackup {
		b := cbt(name, mode, parent, chain, at, ok)
		b.Spec.MachineName = "web"
		b.Spec.Target = s3
		b.Status.Chain.Sequence = seq
		b.Status.StoredBytes = stored
		return b
	}
	other := mk("x", BackupModeFull, "", "c9", 1, 0, 1)
	other.Spec.MachineName = "db"
	onNode := mk("n", BackupModeFull, "", "c9", 1, 0, 1)
	onNode.Spec.Target = nil
	backups := []MachineBackup{
		mk("i2", BackupModeIncremental, "i1", "c1", 3, 2*time.Hour, 3),
		mk("f0", BackupModeFull, "", "c1", 1, 0, 10),
		mk("i1", BackupModeIncremental, "f0", "c1", 2, time.Hour, 2),
		mk("s1", BackupModeSyntheticFull, "i1", "c1", 2, time.Hour+time.Minute, 12),
		mk("g0", BackupModeFull, "", "c2", 1, 5*time.Hour, 20),
		other, onNode,
	}
	chains := BackupChains(backups, "web")
	if len(chains) != 2 || chains[0].ID != "c2" || chains[1].ID != "c1" {
		t.Fatalf("chains = %+v, want c2 then c1", chains)
	}
	c1 := chains[1]
	if c1.StoredBytes != 27 {
		t.Fatalf("stored = %d, want 27", c1.StoredBytes)
	}
	got := make([]string, 0, len(c1.Points))
	depth := map[string]int{}
	for _, p := range c1.Points {
		got = append(got, p.Name)
		depth[p.Name] = p.Depth
	}
	if want := []string{"f0", "i1", "s1", "i2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if depth["f0"] != 0 || depth["i1"] != 1 || depth["s1"] != 0 || depth["i2"] != 2 {
		t.Fatalf("depths = %v", depth)
	}
}
