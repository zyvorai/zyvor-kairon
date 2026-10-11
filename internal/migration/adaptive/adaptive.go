// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package adaptive decides how to steer a running live migration from its
// telemetry. It is pure: Decide maps (settings, previous state, sample,
// now) to the next state and the actions to apply, so kairon-node can
// resume after a restart from the state it persisted in
// MachineMigration.status.adaptive.
//
// Stages escalate one way: PreCopy, then AutoConverge (throttle guest
// vCPUs), then RelaxDowntime (allow a longer final pause, up to
// maxDowntimeMs), then PostCopy (run the guest on the destination and fault
// pages in). Remedies that are disabled or unsupported are skipped. A
// transfer that outlives its completion deadline before post-copy is
// aborted; once post-copy starts it can no longer be aborted.
package adaptive

import (
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	DefaultProgressTimeout         = 30 * time.Second
	DefaultCompletionTimeoutPerGiB = 120 * time.Second
	// DefaultMaxDowntimeMs is QEMU's own downtime-limit default, used as
	// the ceiling when the migration doesn't set maxDowntimeMs.
	DefaultMaxDowntimeMs = 300
	minStartDowntimeMs   = 50

	// minProgressPct is how much remaining RAM must shrink, relative to the
	// last progress marker, to count as progress.
	minProgressPct = 5

	throttleInitialPct   = 20
	throttleIncrementPct = 10
	throttleFirstMaxPct  = 50
	throttleStepPct      = 25
	throttleCeilingPct   = 99

	bandwidthStepInterval = 10 * time.Second
	minBandwidthMbps      = 100

	gib = 1 << 30
)

// UnsupportedMessage is recorded when the adapter can't tune transfers.
const UnsupportedMessage = "adaptive: unsupported by adapter"

// Caps is what this particular transfer allows.
type Caps struct {
	// Tune means the adapter can change a running transfer.
	Tune bool
	// AutoConverge means the stream was started with auto-converge on.
	AutoConverge bool
	// PostCopy means the stream was armed for post-copy and the guest has
	// no host devices that post-copy can't carry.
	PostCopy bool
}

// Sample is one telemetry poll.
type Sample struct {
	RAMTotal           uint64
	RAMRemaining       uint64
	DirtyRateMbps      float64
	ThroughputMbps     float64
	ExpectedDowntimeMs uint64
	CPUThrottlePct     uint64
	PostCopyActive     bool
}

// Settings are the migration's resolved adaptive settings.
type Settings struct {
	Spec          model.MachineMigrationAdaptiveSpec
	MaxDowntimeMs uint64
	Caps          Caps
}

type ActionKind string

const (
	ActionTune     ActionKind = "tune"
	ActionPostCopy ActionKind = "postcopy"
	ActionAbort    ActionKind = "abort"
)

type Action struct {
	Kind   ActionKind
	Tune   migration.TuneRequest
	Reason string
}

// StartDowntimeMs is the downtime limit an adaptive transfer starts with:
// half the ceiling, leaving RelaxDowntime room to trade a longer pause for
// convergence.
func StartDowntimeMs(maxDowntimeMs uint64) uint64 {
	ceiling := maxDowntimeOrDefault(maxDowntimeMs)
	return min(max(ceiling/2, minStartDowntimeMs), ceiling)
}

// Start returns the initial state for a transfer started with the given
// bandwidth and downtime limit.
func Start(s Settings, bandwidthMbps, downtimeMs uint64, now time.Time) model.MachineMigrationAdaptiveStatus {
	t := now.UTC()
	st := model.MachineMigrationAdaptiveStatus{
		Stage:           model.AdaptiveStagePreCopy,
		StartedAt:       &t,
		StageSince:      &t,
		BestProgressAt:  &t,
		BandwidthMbps:   bandwidthMbps,
		DowntimeLimitMs: downtimeMs,
		PostCopyArmed:   s.Caps.PostCopy,
	}
	if !s.Caps.Tune && !s.Caps.PostCopy {
		st.Stage = model.AdaptiveStageUnsupported
		st.Message = UnsupportedMessage
	}
	return st
}

// Decide returns the next state and the actions to apply for sample.
func Decide(s Settings, st model.MachineMigrationAdaptiveStatus, sample Sample, now time.Time) (model.MachineMigrationAdaptiveStatus, []Action) {
	now = now.UTC()
	if st.StartedAt == nil {
		st = Start(s, st.BandwidthMbps, st.DowntimeLimitMs, now)
	}
	if sample.PostCopyActive {
		st.PostCopyStarted = true
	}
	if st.PostCopyStarted || st.Stage == model.AdaptiveStagePostCopy {
		if st.Stage != model.AdaptiveStagePostCopy {
			setStage(&st, model.AdaptiveStagePostCopy, now)
		}
		st.Message = "post-copy: the guest runs on the destination while remaining pages are fetched"
		return st, nil
	}
	if st.Stage == model.AdaptiveStageUnsupported || st.Stage == model.AdaptiveStageAbort {
		return st, nil
	}

	if deadline := completionDeadline(s.Spec, sample.RAMTotal); deadline > 0 && now.Sub(*st.StartedAt) >= deadline {
		setStage(&st, model.AdaptiveStageAbort, now)
		reason := fmt.Sprintf("no convergence within the %s completion deadline", deadline)
		st.Message = reason + "; aborting with the source left running"
		record(&st, now, string(ActionAbort), reason)
		return st, []Action{{Kind: ActionAbort, Reason: reason}}
	}

	progressed := trackProgress(&st, sample.RAMRemaining, now)
	if trigger := postCopyTrigger(s.Spec); trigger > 0 && s.Caps.PostCopy && now.Sub(*st.StartedAt) >= trigger {
		return switchToPostCopy(st, now, fmt.Sprintf("transfer ran longer than postCopy.triggerAfter (%s)", trigger))
	}

	var actions []Action
	var tune migration.TuneRequest
	if reason := stallReason(s, st, sample, progressed, now); reason != "" {
		var postCopy bool
		st, tune, postCopy = escalate(s, st, sample, now, reason)
		if postCopy {
			return switchToPostCopy(st, now, reason)
		}
	}
	if s.Caps.Tune {
		adaptBandwidth(s, &st, sample, now, &tune)
	}
	if !tune.IsZero() {
		actions = append(actions, Action{Kind: ActionTune, Tune: tune, Reason: st.Message})
	}
	return st, actions
}

// trackProgress moves the progress marker when remaining RAM shrank by at
// least minProgressPct since the last marker. Pre-copy's remaining RAM
// bounces up on every dirty-sync pass, so the marker only ever moves down.
func trackProgress(st *model.MachineMigrationAdaptiveStatus, remaining uint64, now time.Time) bool {
	if remaining == 0 {
		return false
	}
	if st.BestRAMRemaining == 0 || remaining*100 <= st.BestRAMRemaining*(100-minProgressPct) {
		st.BestRAMRemaining = remaining
		t := now
		st.BestProgressAt = &t
		return true
	}
	return false
}

func stallReason(s Settings, st model.MachineMigrationAdaptiveStatus, sample Sample, progressed bool, now time.Time) string {
	timeout := progressTimeout(s.Spec)
	if !progressed && st.BestProgressAt != nil && now.Sub(*st.BestProgressAt) >= timeout {
		return fmt.Sprintf("remaining RAM has not shrunk %d%% in %s", minProgressPct, timeout)
	}
	// Rate signals only escalate out of plain pre-copy; later remedies get
	// a full progress timeout to work before the next escalation.
	if st.Stage != model.AdaptiveStagePreCopy {
		return ""
	}
	if sample.DirtyRateMbps > 0 && sample.ThroughputMbps > 0 && sample.DirtyRateMbps >= sample.ThroughputMbps {
		return fmt.Sprintf("guest dirties memory at %.0f Mbps, at or above the %.0f Mbps transfer rate", sample.DirtyRateMbps, sample.ThroughputMbps)
	}
	if ac := s.Spec.AutoConverge; ac != nil && ac.Enabled && ac.DirtyRateThresholdMbps > 0 && sample.DirtyRateMbps >= float64(ac.DirtyRateThresholdMbps) {
		return fmt.Sprintf("guest dirty rate %.0f Mbps reached autoConverge.dirtyRateThresholdMbps (%d)", sample.DirtyRateMbps, ac.DirtyRateThresholdMbps)
	}
	return ""
}

// escalate applies the strongest-needed next remedy for a stall: deepen the
// current stage if it has room, else move to the next available stage.
func escalate(s Settings, st model.MachineMigrationAdaptiveStatus, sample Sample, now time.Time, reason string) (model.MachineMigrationAdaptiveStatus, migration.TuneRequest, bool) {
	ceiling := maxDowntimeOrDefault(s.MaxDowntimeMs)
	canConverge := s.Caps.Tune && s.Caps.AutoConverge && s.Spec.AutoConverge != nil && s.Spec.AutoConverge.Enabled
	canRelax := s.Caps.Tune && st.DowntimeLimitMs < ceiling
	canPostCopy := s.Caps.PostCopy && s.Spec.PostCopy != nil && s.Spec.PostCopy.Enabled

	var tune migration.TuneRequest
	stage := st.Stage
	for {
		switch stage {
		case model.AdaptiveStagePreCopy:
			if canConverge {
				setStage(&st, model.AdaptiveStageAutoConverge, now)
				st.MaxThrottlePct = throttleFirstMaxPct
				tune.CPUThrottleInitialPct = throttleInitialPct
				tune.CPUThrottleIncrementPct = throttleIncrementPct
				tune.MaxCPUThrottlePct = st.MaxThrottlePct
				st.Message = reason + fmt.Sprintf("; throttling guest vCPUs up to %d%%", st.MaxThrottlePct)
				record(&st, now, "auto-converge", st.Message)
				return st, tune, false
			}
			stage = model.AdaptiveStageAutoConverge
		case model.AdaptiveStageAutoConverge:
			if st.Stage == model.AdaptiveStageAutoConverge && st.MaxThrottlePct < throttleCeilingPct {
				st.MaxThrottlePct = min(st.MaxThrottlePct+throttleStepPct, throttleCeilingPct)
				tune.MaxCPUThrottlePct = st.MaxThrottlePct
				resetProgressClock(&st, now)
				st.Message = reason + fmt.Sprintf("; raising the vCPU throttle ceiling to %d%%", st.MaxThrottlePct)
				record(&st, now, "throttle", st.Message)
				return st, tune, false
			}
			if canRelax {
				setStage(&st, model.AdaptiveStageRelaxDowntime, now)
				return relaxDowntime(st, sample, ceiling, now, reason)
			}
			stage = model.AdaptiveStageRelaxDowntime
		case model.AdaptiveStageRelaxDowntime:
			if st.Stage == model.AdaptiveStageRelaxDowntime && canRelax {
				return relaxDowntime(st, sample, ceiling, now, reason)
			}
			if canPostCopy {
				return st, tune, true
			}
			st.Message = reason + "; no remedies left, waiting for convergence or the completion deadline"
			resetProgressClock(&st, now)
			return st, tune, false
		default:
			return st, tune, false
		}
	}
}

// relaxDowntime raises the downtime limit straight to what QEMU expects the
// final pause to need when that fits under the ceiling, else doubles it.
func relaxDowntime(st model.MachineMigrationAdaptiveStatus, sample Sample, ceiling uint64, now time.Time, reason string) (model.MachineMigrationAdaptiveStatus, migration.TuneRequest, bool) {
	next := st.DowntimeLimitMs * 2
	if want := sample.ExpectedDowntimeMs + sample.ExpectedDowntimeMs/10; sample.ExpectedDowntimeMs > st.DowntimeLimitMs && want <= ceiling {
		next = want
	}
	next = min(max(next, minStartDowntimeMs), ceiling)
	st.DowntimeLimitMs = next
	resetProgressClock(&st, now)
	st.Message = reason + fmt.Sprintf("; allowing up to %d ms of final downtime", next)
	record(&st, now, "relax-downtime", st.Message)
	return st, migration.TuneRequest{MaxDowntimeMs: next}, false
}

func switchToPostCopy(st model.MachineMigrationAdaptiveStatus, now time.Time, reason string) (model.MachineMigrationAdaptiveStatus, []Action) {
	setStage(&st, model.AdaptiveStagePostCopy, now)
	st.Message = reason + "; switching to post-copy"
	record(&st, now, string(ActionPostCopy), st.Message)
	return st, []Action{{Kind: ActionPostCopy, Reason: st.Message}}
}

// adaptBandwidth raises the bandwidth cap while throughput keeps up with
// it and backs off when throughput falls well short, at most once per
// bandwidthStepInterval.
func adaptBandwidth(s Settings, st *model.MachineMigrationAdaptiveStatus, sample Sample, now time.Time, tune *migration.TuneRequest) {
	bw := s.Spec.Bandwidth
	if bw == nil || !bw.Adaptive || bw.MaxMbps == 0 || st.BandwidthMbps == 0 || sample.ThroughputMbps <= 0 {
		return
	}
	if st.LastBandwidthChangeAt != nil && now.Sub(*st.LastBandwidthChangeAt) < bandwidthStepInterval {
		return
	}
	cur := float64(st.BandwidthMbps)
	var next uint64
	switch {
	case sample.ThroughputMbps >= cur*0.9 && st.BandwidthMbps < bw.MaxMbps:
		next = min(st.BandwidthMbps+max(st.BandwidthMbps/2, 1), bw.MaxMbps)
	case sample.ThroughputMbps < cur*0.5:
		next = max(uint64(sample.ThroughputMbps*1.25), minBandwidthMbps)
		if next >= st.BandwidthMbps {
			return
		}
	default:
		return
	}
	t := now
	st.LastBandwidthChangeAt = &t
	record(st, now, "bandwidth", fmt.Sprintf("bandwidth %d -> %d Mbps at %.0f Mbps throughput", st.BandwidthMbps, next, sample.ThroughputMbps))
	st.BandwidthMbps = next
	tune.BandwidthMbps = next
}

func setStage(st *model.MachineMigrationAdaptiveStatus, stage string, now time.Time) {
	st.Stage = stage
	t := now
	st.StageSince = &t
	resetProgressClock(st, now)
}

// resetProgressClock gives a freshly applied remedy a full progress
// timeout before it can be judged.
func resetProgressClock(st *model.MachineMigrationAdaptiveStatus, now time.Time) {
	t := now
	st.BestProgressAt = &t
}

func record(st *model.MachineMigrationAdaptiveStatus, now time.Time, action, detail string) {
	st.Actions = append(st.Actions, model.MachineMigrationAdaptiveAction{At: now, Action: action, Detail: detail})
	if n := len(st.Actions); n > model.MaxAdaptiveActions {
		st.Actions = append([]model.MachineMigrationAdaptiveAction(nil), st.Actions[n-model.MaxAdaptiveActions:]...)
	}
}

// Record appends an entry to the capped action log, for outcomes the
// caller observes (such as an action that failed to apply).
func Record(st *model.MachineMigrationAdaptiveStatus, now time.Time, action, detail string) {
	record(st, now.UTC(), action, detail)
}

func completionDeadline(spec model.MachineMigrationAdaptiveSpec, ramTotal uint64) time.Duration {
	if ramTotal == 0 {
		return 0
	}
	perGiB := parseDuration(spec.CompletionTimeoutPerGiB, DefaultCompletionTimeoutPerGiB)
	gibs := max((ramTotal+gib-1)/gib, 1)
	return time.Duration(gibs) * perGiB
}

func progressTimeout(spec model.MachineMigrationAdaptiveSpec) time.Duration {
	return parseDuration(spec.ProgressTimeout, DefaultProgressTimeout)
}

func postCopyTrigger(spec model.MachineMigrationAdaptiveSpec) time.Duration {
	if spec.PostCopy == nil || !spec.PostCopy.Enabled {
		return 0
	}
	return parseDuration(spec.PostCopy.TriggerAfter, 0)
}

func parseDuration(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func maxDowntimeOrDefault(v uint64) uint64 {
	if v == 0 {
		return DefaultMaxDowntimeMs
	}
	return v
}
