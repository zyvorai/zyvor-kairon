// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func webPolicy(name string, maxConcurrent int, bandwidthMbps uint64) model.MigrationPolicy {
	return model.MigrationPolicy{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MigrationPolicySpec{Selector: map[string]string{"tier": "web"}, MaxConcurrent: maxConcurrent, BandwidthMbps: bandwidthMbps},
	}
}

func activeMigrationFor(machineName string) model.MachineMigration {
	return model.MachineMigration{
		Metadata: model.ObjectMeta{Name: "mig-" + machineName, Namespace: "prod"},
		Spec:     model.MachineMigrationSpec{MachineName: machineName},
		Status:   model.MachineMigrationStatus{Phase: "Running"},
	}
}

func TestAdmitMigrationPolicyAllowsUntilMaxConcurrentExhausted(t *testing.T) {
	machines := []model.Machine{webMachine("web-1", "node-a", "Running"), webMachine("web-2", "node-a", "Running")}
	migrations := []model.MachineMigration{activeMigrationFor("web-1")}
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 1, 0)}, machines, migrations)
	if blocker := AdmitMigrationPolicy(states, machines[1]); blocker == "" {
		t.Fatal("expected the second concurrent migration to be blocked (maxConcurrent=1, one already active)")
	}
}

func TestAdmitMigrationPolicyAllowsBelowCap(t *testing.T) {
	machines := []model.Machine{webMachine("web-1", "node-a", "Running"), webMachine("web-2", "node-a", "Running")}
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 2, 0)}, machines, nil)
	if blocker := AdmitMigrationPolicy(states, machines[0]); blocker != "" {
		t.Fatalf("expected the first migration to be admitted, got %q", blocker)
	}
	if blocker := AdmitMigrationPolicy(states, machines[1]); blocker != "" {
		t.Fatalf("expected the second migration to be admitted (maxConcurrent=2), got %q", blocker)
	}
}

func TestAdmitMigrationPolicyZeroMaxConcurrentIsUnlimited(t *testing.T) {
	machine := webMachine("web-1", "node-a", "Running")
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 0, 0)}, []model.Machine{machine}, nil)
	for i := 0; i < 10; i++ {
		if blocker := AdmitMigrationPolicy(states, machine); blocker != "" {
			t.Fatalf("expected maxConcurrent=0 to mean unlimited, got blocked on iteration %d: %q", i, blocker)
		}
	}
}

func TestAdmitMigrationPolicyIgnoresNonMatchingMachine(t *testing.T) {
	other := model.Machine{Metadata: model.ObjectMeta{Name: "db-1", Namespace: "prod", Labels: map[string]string{"tier": "db"}}}
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 0, 0)}, []model.Machine{other}, nil)
	if blocker := AdmitMigrationPolicy(states, other); blocker != "" {
		t.Fatalf("expected a non-matching Machine to be unaffected, got %q", blocker)
	}
}

func TestBandwidthMbpsFromPoliciesReturnsFirstMatch(t *testing.T) {
	machine := webMachine("web-1", "node-a", "Running")
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 0, 500)}, []model.Machine{machine}, nil)
	if bw := BandwidthMbpsFromPolicies(states, machine); bw != 500 {
		t.Fatalf("expected bandwidth 500, got %d", bw)
	}
}

func TestBandwidthMbpsFromPoliciesReturnsZeroWhenNoneMatch(t *testing.T) {
	other := model.Machine{Metadata: model.ObjectMeta{Name: "db-1", Namespace: "prod", Labels: map[string]string{"tier": "db"}}}
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 0, 500)}, []model.Machine{other}, nil)
	if bw := BandwidthMbpsFromPolicies(states, other); bw != 0 {
		t.Fatalf("expected 0 for a non-matching Machine, got %d", bw)
	}
}

func adaptivePolicy() model.MigrationPolicy {
	p := webPolicy("adaptive-mp", 0, 0)
	p.Spec.Strategy = model.MigrationTuningAdaptive
	p.Spec.MaxDowntimeMs = 250
	p.Spec.CompletionTimeoutPerGiB = "120s"
	p.Spec.ProgressTimeout = "30s"
	p.Spec.AutoConverge = &model.MigrationAutoConverge{Enabled: true, DirtyRateThresholdMbps: 750}
	p.Spec.PostCopy = &model.MigrationPostCopy{Enabled: true, TriggerAfter: "90s"}
	p.Spec.Compression = &model.MigrationCompression{Algorithm: "zstd", Level: 3}
	p.Spec.Bandwidth = &model.MigrationBandwidth{MaxMbps: 10000, Adaptive: true}
	return p
}

func TestResolveMigrationTuningSnapshotsAdaptivePolicy(t *testing.T) {
	machine := webMachine("web-1", "node-a", "Running")
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{adaptivePolicy()}, []model.Machine{machine}, nil)
	tuning := ResolveMigrationTuning(states, machine)
	if tuning.BandwidthMbps != 10000 || tuning.MaxDowntimeMs != 250 || tuning.Adaptive == nil || tuning.Adaptive.Policy != "adaptive-mp" {
		t.Fatalf("tuning = %+v", tuning)
	}
	patch, err := MigrationTuningPatch(model.MachineMigrationSpec{MachineName: "web-1"}, tuning)
	if err != nil {
		t.Fatal(err)
	}
	if patch["bandwidthMbps"] != uint64(2500) {
		t.Fatalf("adaptive bandwidth must start at a quarter of maxMbps, got %v", patch["bandwidthMbps"])
	}
	if patch["maxDowntimeMs"] != uint64(250) || patch["multifdChannels"] != defaultCompressionMultifdChannels {
		t.Fatalf("patch = %v", patch)
	}
	a, ok := patch["adaptive"].(*model.MachineMigrationAdaptiveSpec)
	if !ok || a.ProgressTimeout != "30s" || a.Compression.Algorithm != "zstd" {
		t.Fatalf("adaptive patch = %#v", patch["adaptive"])
	}
}

func TestMigrationTuningPatchExplicitValuesWin(t *testing.T) {
	machine := webMachine("web-1", "node-a", "Running")
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{adaptivePolicy()}, []model.Machine{machine}, nil)
	spec := model.MachineMigrationSpec{
		MachineName: "web-1", BandwidthMbps: 800, MaxDowntimeMs: 50, MultifdChannels: 8,
		Adaptive: &model.MachineMigrationAdaptiveSpec{ProgressTimeout: "10s", PostCopy: &model.MigrationPostCopy{Enabled: false}},
	}
	patch, err := MigrationTuningPatch(spec, ResolveMigrationTuning(states, machine))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"bandwidthMbps", "maxDowntimeMs", "multifdChannels"} {
		if _, ok := patch[k]; ok {
			t.Fatalf("explicit %s overwritten: %v", k, patch)
		}
	}
	a := patch["adaptive"].(*model.MachineMigrationAdaptiveSpec)
	if a.ProgressTimeout != "10s" || a.PostCopy.Enabled || a.CompletionTimeoutPerGiB != "120s" {
		t.Fatalf("merge = %+v", a)
	}
}

func TestMigrationTuningPatchStaticPolicyOnlySetsBandwidth(t *testing.T) {
	machine := webMachine("web-1", "node-a", "Running")
	states := LoadMigrationPolicyStates([]model.MigrationPolicy{webPolicy("web-mp", 0, 500)}, []model.Machine{machine}, nil)
	patch, err := MigrationTuningPatch(model.MachineMigrationSpec{MachineName: "web-1"}, ResolveMigrationTuning(states, machine))
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) != 1 || patch["bandwidthMbps"] != uint64(500) {
		t.Fatalf("patch = %v", patch)
	}
}

func TestMigrationTuningPatchRejectsBadDurations(t *testing.T) {
	spec := model.MachineMigrationSpec{MachineName: "web-1", Adaptive: &model.MachineMigrationAdaptiveSpec{ProgressTimeout: "soon"}}
	if _, err := MigrationTuningPatch(spec, MigrationTuning{}); err == nil {
		t.Fatal("expected an invalid progressTimeout to be rejected")
	}
}
