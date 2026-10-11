// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package adaptive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func fullSettings() Settings {
	return Settings{
		Spec: model.MachineMigrationAdaptiveSpec{
			ProgressTimeout:         "30s",
			CompletionTimeoutPerGiB: "120s",
			AutoConverge:            &model.MigrationAutoConverge{Enabled: true},
			PostCopy:                &model.MigrationPostCopy{Enabled: true},
		},
		MaxDowntimeMs: 400,
		Caps:          Caps{Tune: true, AutoConverge: true, PostCopy: true},
	}
}

const ram = 4 << 30

func stalled(remaining uint64) Sample {
	return Sample{RAMTotal: ram, RAMRemaining: remaining, DirtyRateMbps: 100, ThroughputMbps: 1000}
}

func TestStartDowntimeMs(t *testing.T) {
	for _, tc := range []struct{ max, want uint64 }{{0, 150}, {400, 200}, {60, 50}, {30, 30}} {
		if got := StartDowntimeMs(tc.max); got != tc.want {
			t.Errorf("StartDowntimeMs(%d) = %d, want %d", tc.max, got, tc.want)
		}
	}
}

func TestStartWithoutTunerOrPostCopyIsUnsupported(t *testing.T) {
	st := Start(Settings{}, 1000, 150, t0)
	if st.Stage != model.AdaptiveStageUnsupported || st.Message != UnsupportedMessage {
		t.Fatalf("st = %+v", st)
	}
	st, actions := Decide(Settings{}, st, stalled(ram), t0.Add(time.Hour))
	if len(actions) != 0 || st.Stage != model.AdaptiveStageUnsupported {
		t.Fatalf("unsupported must stay inert, got %+v %+v", st, actions)
	}
}

// TestStallEscalation drives a transfer whose remaining RAM never shrinks
// through every stage in order.
func TestStallEscalation(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, StartDowntimeMs(s.MaxDowntimeMs), t0)
	st, actions := Decide(s, st, stalled(ram/2), t0)
	if len(actions) != 0 || st.BestRAMRemaining != ram/2 {
		t.Fatalf("first sample: %+v %+v", st, actions)
	}
	steps := []struct {
		stage string
		check func(Action) bool
	}{
		{model.AdaptiveStageAutoConverge, func(a Action) bool {
			return a.Kind == ActionTune && a.Tune.MaxCPUThrottlePct == 50 && a.Tune.CPUThrottleInitialPct == 20
		}},
		{model.AdaptiveStageAutoConverge, func(a Action) bool { return a.Kind == ActionTune && a.Tune.MaxCPUThrottlePct == 75 }},
		{model.AdaptiveStageAutoConverge, func(a Action) bool { return a.Kind == ActionTune && a.Tune.MaxCPUThrottlePct == 99 }},
		{model.AdaptiveStageRelaxDowntime, func(a Action) bool { return a.Kind == ActionTune && a.Tune.MaxDowntimeMs == 400 }},
		{model.AdaptiveStagePostCopy, func(a Action) bool { return a.Kind == ActionPostCopy }},
	}
	now := t0
	for i, step := range steps {
		now = now.Add(31 * time.Second)
		st, actions = Decide(s, st, stalled(ram/2), now)
		if st.Stage != step.stage || len(actions) != 1 || !step.check(actions[0]) {
			t.Fatalf("step %d: stage=%s actions=%+v", i, st.Stage, actions)
		}
	}
	if len(st.Actions) != len(steps) {
		t.Fatalf("action log = %+v", st.Actions)
	}
	if _, actions = Decide(s, st, stalled(ram/2), now.Add(time.Minute)); len(actions) != 0 {
		t.Fatalf("post-copy is terminal for the engine, got %+v", actions)
	}
}

func TestProgressKeepsPreCopy(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, 200, t0)
	remaining := uint64(ram)
	now := t0
	for i := 0; i < 10; i++ {
		now = now.Add(20 * time.Second)
		remaining = remaining * 90 / 100
		var actions []Action
		st, actions = Decide(s, st, stalled(remaining), now)
		if len(actions) != 0 || st.Stage != model.AdaptiveStagePreCopy {
			t.Fatalf("tick %d: progressing transfer escalated: %+v %+v", i, st, actions)
		}
	}
}

func TestSlowCreepAccumulatesIntoProgress(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, 200, t0)
	st, _ = Decide(s, st, stalled(1000<<20), t0)
	// 2% per 10s: no single tick is 5%, but the marker compares against
	// the last marker, so every third tick counts.
	remaining := uint64(1000 << 20)
	now := t0
	for i := 0; i < 12; i++ {
		now = now.Add(10 * time.Second)
		remaining = remaining * 98 / 100
		var actions []Action
		st, actions = Decide(s, st, stalled(remaining), now)
		if len(actions) != 0 {
			t.Fatalf("tick %d escalated a slowly converging transfer: %+v", i, actions)
		}
	}
}

func TestDirtyRateAboveThroughputEscalatesImmediately(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, 200, t0)
	sample := Sample{RAMTotal: ram, RAMRemaining: ram / 2, DirtyRateMbps: 1200, ThroughputMbps: 900}
	st, actions := Decide(s, st, sample, t0.Add(time.Second))
	if st.Stage != model.AdaptiveStageAutoConverge || len(actions) != 1 || !strings.Contains(st.Message, "dirties memory") {
		t.Fatalf("st=%+v actions=%+v", st, actions)
	}
	// In AutoConverge the rate signal no longer escalates on its own.
	if st, actions = Decide(s, st, sample, t0.Add(2*time.Second)); len(actions) != 0 || st.MaxThrottlePct != 50 {
		t.Fatalf("rate signal re-escalated: %+v %+v", st, actions)
	}
}

func TestDirtyRateThreshold(t *testing.T) {
	s := fullSettings()
	s.Spec.AutoConverge.DirtyRateThresholdMbps = 500
	st := Start(s, 1000, 200, t0)
	sample := Sample{RAMTotal: ram, RAMRemaining: ram / 2, DirtyRateMbps: 600, ThroughputMbps: 5000}
	if st, _ = Decide(s, st, sample, t0.Add(time.Second)); st.Stage != model.AdaptiveStageAutoConverge {
		t.Fatalf("threshold did not trigger auto-converge: %+v", st)
	}
}

func TestDisabledRemediesAreSkipped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Settings)
		stage  string
		kind   ActionKind
	}{
		{"no auto-converge goes to relax", func(s *Settings) { s.Spec.AutoConverge.Enabled = false }, model.AdaptiveStageRelaxDowntime, ActionTune},
		{"stream without auto-converge goes to relax", func(s *Settings) { s.Caps.AutoConverge = false }, model.AdaptiveStageRelaxDowntime, ActionTune},
		{"no tuner goes straight to post-copy", func(s *Settings) { s.Caps.Tune = false }, model.AdaptiveStagePostCopy, ActionPostCopy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fullSettings()
			tc.mutate(&s)
			st := Start(s, 1000, 200, t0)
			st, _ = Decide(s, st, stalled(ram/2), t0)
			st, actions := Decide(s, st, stalled(ram/2), t0.Add(31*time.Second))
			if st.Stage != tc.stage || len(actions) != 1 || actions[0].Kind != tc.kind {
				t.Fatalf("stage=%s actions=%+v", st.Stage, actions)
			}
		})
	}
}

func TestNoRemediesLeftWaitsForDeadline(t *testing.T) {
	s := fullSettings()
	s.Spec.AutoConverge = nil
	s.Spec.PostCopy = nil
	s.MaxDowntimeMs = 200
	st := Start(s, 1000, 200, t0)
	st, _ = Decide(s, st, stalled(ram/2), t0)
	st, actions := Decide(s, st, stalled(ram/2), t0.Add(31*time.Second))
	if len(actions) != 0 || !strings.Contains(st.Message, "no remedies left") {
		t.Fatalf("st=%+v actions=%+v", st, actions)
	}
	// 4 GiB x 120s = 8m.
	st, actions = Decide(s, st, stalled(ram/2), t0.Add(8*time.Minute))
	if st.Stage != model.AdaptiveStageAbort || len(actions) != 1 || actions[0].Kind != ActionAbort {
		t.Fatalf("deadline: st=%+v actions=%+v", st, actions)
	}
	if _, actions = Decide(s, st, stalled(ram/2), t0.Add(9*time.Minute)); len(actions) != 0 {
		t.Fatalf("abort must be emitted once, got %+v", actions)
	}
}

func TestRelaxDowntimeJumpsToExpectedDowntime(t *testing.T) {
	s := fullSettings()
	s.Spec.AutoConverge = nil
	s.MaxDowntimeMs = 1000
	st := Start(s, 1000, 200, t0)
	st, _ = Decide(s, st, stalled(ram/2), t0)
	sample := stalled(ram / 2)
	sample.ExpectedDowntimeMs = 600
	st, actions := Decide(s, st, sample, t0.Add(31*time.Second))
	if st.Stage != model.AdaptiveStageRelaxDowntime || actions[0].Tune.MaxDowntimeMs != 660 {
		t.Fatalf("st=%+v actions=%+v", st, actions)
	}
}

func TestPostCopyActiveLatchesAndNeverAborts(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, 200, t0)
	sample := stalled(ram / 2)
	sample.PostCopyActive = true
	st, actions := Decide(s, st, sample, t0.Add(time.Second))
	if !st.PostCopyStarted || st.Stage != model.AdaptiveStagePostCopy || len(actions) != 0 {
		t.Fatalf("st=%+v actions=%+v", st, actions)
	}
	sample.PostCopyActive = false
	st, actions = Decide(s, st, sample, t0.Add(24*time.Hour))
	if !st.PostCopyStarted || len(actions) != 0 {
		t.Fatalf("post-copy must stay latched with no abort past the deadline: %+v %+v", st, actions)
	}
}

func TestTriggerAfterSwitchesToPostCopy(t *testing.T) {
	s := fullSettings()
	s.Spec.PostCopy.TriggerAfter = "90s"
	st := Start(s, 1000, 200, t0)
	remaining := uint64(ram)
	now := t0
	var actions []Action
	for now.Sub(t0) < 90*time.Second {
		now = now.Add(15 * time.Second)
		remaining = remaining * 90 / 100
		st, actions = Decide(s, st, stalled(remaining), now)
	}
	if st.Stage != model.AdaptiveStagePostCopy || len(actions) != 1 || actions[0].Kind != ActionPostCopy {
		t.Fatalf("st=%+v actions=%+v", st, actions)
	}
}

func TestTriggerAfterNeedsArmedStream(t *testing.T) {
	s := fullSettings()
	s.Spec.PostCopy.TriggerAfter = "10s"
	s.Caps.PostCopy = false
	st := Start(s, 1000, 200, t0)
	st, actions := Decide(s, st, stalled(ram/2), t0.Add(15*time.Second))
	if st.Stage == model.AdaptiveStagePostCopy || len(actions) != 0 {
		t.Fatalf("switched without an armed stream: %+v %+v", st, actions)
	}
}

func TestAdaptiveBandwidth(t *testing.T) {
	s := fullSettings()
	s.Spec.Bandwidth = &model.MigrationBandwidth{MaxMbps: 4000, Adaptive: true}
	st := Start(s, 1000, 200, t0)
	sample := Sample{RAMTotal: ram, RAMRemaining: ram, ThroughputMbps: 950}
	var actions []Action
	now := t0
	for _, want := range []uint64{1500, 2250, 3375, 4000} {
		now = now.Add(11 * time.Second)
		sample.RAMRemaining = sample.RAMRemaining * 80 / 100
		sample.ThroughputMbps = float64(st.BandwidthMbps) * 0.95
		st, actions = Decide(s, st, sample, now)
		if len(actions) != 1 || actions[0].Tune.BandwidthMbps != want {
			t.Fatalf("want %d, got %+v", want, actions)
		}
	}
	// Paced: no change within the step interval.
	sample.ThroughputMbps = 500
	sample.RAMRemaining = sample.RAMRemaining * 80 / 100
	if st, actions = Decide(s, st, sample, now.Add(time.Second)); len(actions) != 0 {
		t.Fatalf("change inside the step interval: %+v", actions)
	}
	sample.RAMRemaining = sample.RAMRemaining * 80 / 100
	st, actions = Decide(s, st, sample, now.Add(11*time.Second))
	if len(actions) != 1 || actions[0].Tune.BandwidthMbps != 625 || st.BandwidthMbps != 625 {
		t.Fatalf("back-off: %+v %+v", st, actions)
	}
}

func TestActionLogIsCapped(t *testing.T) {
	var st model.MachineMigrationAdaptiveStatus
	for i := 0; i < model.MaxAdaptiveActions+5; i++ {
		Record(&st, t0.Add(time.Duration(i)*time.Second), "x", "")
	}
	if len(st.Actions) != model.MaxAdaptiveActions || !st.Actions[0].At.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("log = %d entries, first %v", len(st.Actions), st.Actions[0].At)
	}
}

func TestDecideResumesFromPersistedState(t *testing.T) {
	s := fullSettings()
	st := Start(s, 1000, 200, t0)
	st, _ = Decide(s, st, stalled(ram/2), t0)
	// A restarted node only has what was persisted; the stall clock and
	// marker survive the round trip.
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.MachineMigrationAdaptiveStatus
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	_, actions := Decide(s, restored, stalled(ram/2), t0.Add(31*time.Second))
	if len(actions) != 1 || actions[0].Kind != ActionTune {
		t.Fatalf("restored state did not resume: %+v", actions)
	}
}
