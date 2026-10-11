// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func parseMigrateFlags(t *testing.T, args ...string) migrateFlags {
	t.Helper()
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	f := migrateFlags{}
	fs.StringVar(&f.strategy, "strategy", "auto", "")
	fs.UintVar(&f.multifd, "multifd-channels", 0, "")
	fs.StringVar(&f.progressTimeout, "progress-timeout", "", "")
	fs.BoolVar(&f.postCopy, "post-copy", false, "")
	fs.StringVar(&f.compression, "compression", "", "")
	fs.UintVar(&f.compressionLevel, "compression-level", 0, "")
	fs.BoolVar(&f.autoConverge, "auto-converge", false, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	f.set = visited(fs)
	return f
}

func TestMigrateAdaptiveBuildsLiveSpecWithOnlyGivenOverrides(t *testing.T) {
	spec, err := parseMigrateFlags(t, "--strategy", "adaptive", "--progress-timeout", "20s", "--post-copy", "--compression", "zstd", "--compression-level", "3").spec("db")
	if err != nil {
		t.Fatal(err)
	}
	a := spec.Adaptive
	if spec.Strategy != "live" || a == nil || a.ProgressTimeout != "20s" || a.PostCopy == nil || !a.PostCopy.Enabled || a.Compression.Level != 3 {
		t.Fatalf("spec = %+v adaptive = %+v", spec, a)
	}
	if a.AutoConverge != nil || a.Bandwidth != nil || a.CompletionTimeoutPerGiB != "" {
		t.Fatalf("unset flags must be left for the policy: %+v", a)
	}
	if spec.MultifdChannels != 2 {
		t.Fatalf("compression must default multifd, got %d", spec.MultifdChannels)
	}
}

func TestMigrateAdaptiveFlagsNeedAdaptiveStrategy(t *testing.T) {
	if _, err := parseMigrateFlags(t, "--strategy", "live", "--post-copy").spec("db"); err == nil || !strings.Contains(err.Error(), "--strategy adaptive") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrateAdaptiveRejectsBadDuration(t *testing.T) {
	if _, err := parseMigrateFlags(t, "--strategy", "adaptive", "--progress-timeout", "later").spec("db"); err == nil {
		t.Fatal("expected a bad duration to be rejected")
	}
}

func TestPolicyTuningPatchOnlyIncludesGivenFlags(t *testing.T) {
	fs := flag.NewFlagSet("p", flag.ContinueOnError)
	var f policyTuningFlags
	f.register(fs)
	if err := fs.Parse([]string{"--strategy", "adaptive", "--post-copy-after", "90s", "--max-bandwidth-mbps", "10000"}); err != nil {
		t.Fatal(err)
	}
	patch, err := f.patch(visited(fs))
	if err != nil {
		t.Fatal(err)
	}
	if patch["strategy"] != "adaptive" || len(patch) != 3 {
		t.Fatalf("patch = %v", patch)
	}
	if pc := patch["postCopy"].(map[string]any); pc["triggerAfter"] != "90s" || pc["enabled"] != nil {
		t.Fatalf("postCopy patch = %v", pc)
	}
	if bw := patch["bandwidth"].(map[string]any); bw["maxMbps"] != uint64(10000) {
		t.Fatalf("bandwidth patch = %v", bw)
	}
}

func TestPolicyTuningPatchValidates(t *testing.T) {
	for _, args := range [][]string{{"--strategy", "fast"}, {"--compression", "lz4"}, {"--progress-timeout", "x"}} {
		fs := flag.NewFlagSet("p", flag.ContinueOnError)
		var f policyTuningFlags
		f.register(fs)
		_ = fs.Parse(args)
		if _, err := f.patch(visited(fs)); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}

func TestMigrationProgressBlock(t *testing.T) {
	if migrationProgressBlock(model.MachineMigrationStatus{Phase: "Pending"}) != "" {
		t.Fatal("a migration without transfer data must print no block")
	}
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	out := migrationProgressBlock(model.MachineMigrationStatus{
		Phase: "Running", RAMTotal: 4 << 30, RAMTransferred: 1 << 30, RAMRemaining: 3 << 30,
		Progress: &model.MachineMigrationProgress{ThroughputMbps: 900, DirtyRateMbps: 300, CompressionRatioPercent: 250, EstimatedCompletionSeconds: 45},
		Adaptive: &model.MachineMigrationAdaptiveStatus{Stage: "AutoConverge", PostCopyStarted: true, Actions: []model.MachineMigrationAdaptiveAction{{At: at, Action: "auto-converge", Detail: "stalled"}}},
	})
	for _, want := range []string{"1.0 GiB of 4.0 GiB (25%)", "900 Mbps", "2.50x", "45s", "AutoConverge", "operator recovery", "2026-10-11T12:00:00Z  auto-converge"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
