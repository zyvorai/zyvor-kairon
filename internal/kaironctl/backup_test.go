// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestPrintBackupChainsDrawsTheTree(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	locked := time.Now().Add(24 * time.Hour)
	var buf bytes.Buffer
	printBackupChains(&buf, []model.BackupChainView{{
		ID:          "c1",
		StoredBytes: 3 << 20,
		Points: []model.BackupChainPoint{
			{Name: "f0", Kind: model.BackupModeFull, Phase: model.BackupSucceeded, Sequence: 1, PointInTime: &at, StoredBytes: 2 << 20},
			{Name: "i1", Kind: model.BackupModeIncremental, Phase: model.BackupSucceeded, Sequence: 2, Depth: 1, StoredBytes: 1 << 20, ChangedBytes: 1 << 20, RetentionUntil: &locked},
			{Name: "s1", Kind: model.BackupModeSyntheticFull, Sequence: 2, Deleting: true},
		},
	}})
	out := buf.String()
	for _, want := range []string{
		"chain c1 (3 backups, 3.0MiB stored)",
		"\n* f0  #1 full " + model.BackupSucceeded + "  2026-10-01T00:00:00Z  stored=2.0MiB",
		"\n  + i1  #2 incremental",
		"changed=1.0MiB locked-until=",
		"\n= s1  #2 synthetic-full -  -",
		" deleting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	printBackupChains(&buf, nil)
	if buf.String() != "no S3 backups\n" {
		t.Fatalf("empty = %q", buf.String())
	}
}
