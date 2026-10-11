// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/migration/adaptive"
	"github.com/zyvorai/kairon/internal/model"
)

// sourceCapabilities asks the source adapter what it can do mid-transfer.
// Adapters without the optional routes report nothing.
func (a *Agent) sourceCapabilities(ctx context.Context) (migration.Capabilities, migration.Tuner) {
	tuner, ok := a.SourceMigrator.(migration.Tuner)
	if !ok {
		return migration.Capabilities{}, nil
	}
	caps, err := tuner.Capabilities(ctx)
	if err != nil {
		a.Log.Warn("source adapter capabilities lookup failed; running without adaptive control", "error", err)
		return migration.Capabilities{}, nil
	}
	return caps, tuner
}

// liveStreamPlan is the stream a live migration asks the destination for,
// plus notes on requested features that had to be dropped.
type liveStreamPlan struct {
	stream migration.StreamCapabilities
	notes  []string
}

func planLiveStream(item model.MachineMigration, machine model.Machine, rec *fluxvm.Record, caps migration.Capabilities, mode string) (liveStreamPlan, error) {
	plan := liveStreamPlan{stream: migration.StreamCapabilities{MultifdChannels: item.Spec.MultifdChannels}}
	if mode == "post-copy" {
		if hostDevices(machine, rec) {
			return plan, fmt.Errorf("post-copy refused: the Machine has host devices, which post-copy can't carry")
		}
		plan.stream.PostCopy = true
	}
	spec := item.Spec.Adaptive
	if spec == nil {
		return plan, nil
	}
	if c := spec.Compression; c != nil && c.Algorithm != "" && c.Algorithm != model.MigrationCompressionNone {
		switch {
		case plan.stream.MultifdChannels == 0:
			plan.notes = append(plan.notes, "compression needs multifd; sending uncompressed")
		case !caps.SupportsCompression(c.Algorithm):
			plan.notes = append(plan.notes, fmt.Sprintf("source adapter can't compress with %s; sending uncompressed", c.Algorithm))
		default:
			comp := &migration.Compression{Algorithm: c.Algorithm}
			if c.Level > 0 {
				level := c.Level
				comp.Level = &level
			}
			plan.stream.Compression = comp
		}
	}
	if p := spec.PostCopy; p != nil && p.Enabled && !plan.stream.PostCopy {
		if reason := postCopyBlocker(machine, rec, caps, plan.stream.MultifdChannels); reason != "" {
			plan.notes = append(plan.notes, "post-copy disabled: "+reason)
		} else {
			plan.stream.PostCopy = true
		}
	}
	return plan, nil
}

func postCopyBlocker(machine model.Machine, rec *fluxvm.Record, caps migration.Capabilities, multifd uint8) string {
	switch {
	case hostDevices(machine, rec):
		return "the Machine has host devices, which post-copy can't carry"
	case !caps.PostCopySwitch:
		return "the source adapter can't switch a running transfer to post-copy"
	case multifd > 0 && !caps.PostCopyWithMultifd:
		return "post-copy with multifd needs QEMU 10.0 or later on the source"
	}
	return ""
}

func hostDevices(machine model.Machine, rec *fluxvm.Record) bool {
	return len(machine.Spec.DeviceClaims) > 0 || (rec != nil && len(rec.Request.VFIODevices) > 0)
}

// sourceStartOptions maps the stream the destination accepted onto source
// options. A nil accepted stream comes from a destination that predates
// stream negotiation; it keeps the legacy multifd-and-mode behaviour.
func sourceStartOptions(item model.MachineMigration, mode string, plan liveStreamPlan, accepted *migration.StreamCapabilities, caps migration.Capabilities) (migration.SourceOptions, []string, error) {
	opts := migration.SourceOptions{
		Mode:            mode,
		BandwidthMbps:   item.Spec.BandwidthMbps,
		MaxDowntimeMs:   item.Spec.MaxDowntimeMs,
		MultifdChannels: item.Spec.MultifdChannels,
	}
	notes := plan.notes
	spec := item.Spec.Adaptive
	if accepted == nil {
		if plan.stream.Compression != nil || (plan.stream.PostCopy && mode != "post-copy") {
			notes = append(notes, "destination predates stream negotiation; compression and armed post-copy are off")
		}
	} else {
		if mode == "post-copy" && !accepted.PostCopy {
			return opts, notes, fmt.Errorf("destination did not enable post-copy for this transfer")
		}
		opts.MultifdChannels = accepted.MultifdChannels
		opts.Compression = accepted.Compression
		if plan.stream.Compression != nil && accepted.Compression == nil {
			notes = append(notes, "destination did not accept compression; sending uncompressed")
		}
		switch {
		case mode == "post-copy":
			opts.PostCopy = migration.PostCopyImmediate
		case accepted.PostCopy:
			opts.PostCopy = migration.PostCopyArmed
		case plan.stream.PostCopy:
			notes = append(notes, "destination did not arm post-copy")
		}
	}
	if spec != nil {
		opts.MaxDowntimeMs = adaptive.StartDowntimeMs(item.Spec.MaxDowntimeMs)
		if autoConvergeAvailable(spec, caps) {
			// Off until the engine tunes it: QEMU's auto-converge capability
			// can only be enabled at stream start.
			opts.AutoConverge = &migration.AutoConverge{InitialPct: 1, IncrementPct: 1, MaxPct: 1}
		}
	}
	return opts, notes, nil
}

func autoConvergeAvailable(spec *model.MachineMigrationAdaptiveSpec, caps migration.Capabilities) bool {
	return spec.AutoConverge != nil && spec.AutoConverge.Enabled && caps.AutoConverge && caps.Tune
}

func adaptiveSettings(item model.MachineMigration, st *model.MachineMigrationAdaptiveStatus, caps migration.Capabilities, tuner migration.Tuner) adaptive.Settings {
	spec := *item.Spec.Adaptive
	return adaptive.Settings{
		Spec:          spec,
		MaxDowntimeMs: item.Spec.MaxDowntimeMs,
		Caps: adaptive.Caps{
			Tune:         tuner != nil && caps.Tune,
			AutoConverge: autoConvergeAvailable(&spec, caps),
			PostCopy:     tuner != nil && st.PostCopyArmed && caps.PostCopySwitch,
		},
	}
}

func adaptiveStart(item model.MachineMigration, opts migration.SourceOptions, caps migration.Capabilities, tuner migration.Tuner, notes []string, now time.Time) *model.MachineMigrationAdaptiveStatus {
	if item.Spec.Adaptive == nil {
		if opts.PostCopy == migration.PostCopyImmediate || opts.Mode == "post-copy" {
			return postCopyLatched(nil, now)
		}
		return nil
	}
	armed := opts.PostCopy == migration.PostCopyArmed
	st := &model.MachineMigrationAdaptiveStatus{PostCopyArmed: armed}
	settings := adaptiveSettings(item, st, caps, tuner)
	started := adaptive.Start(settings, opts.BandwidthMbps, opts.MaxDowntimeMs, now)
	st = &started
	if opts.PostCopy == migration.PostCopyImmediate {
		st = postCopyLatched(st, now)
	}
	if len(notes) > 0 {
		msg := strings.Join(notes, "; ")
		if st.Message != "" {
			msg = st.Message + "; " + msg
		}
		st.Message = msg
	}
	return st
}

// postCopyLatched marks post-copy as started. Immediate post-copy is
// latched at transfer start: QEMU switches as soon as the stream is up.
func postCopyLatched(st *model.MachineMigrationAdaptiveStatus, now time.Time) *model.MachineMigrationAdaptiveStatus {
	if st == nil {
		st = &model.MachineMigrationAdaptiveStatus{}
	}
	if st.PostCopyStarted {
		return st
	}
	t := now.UTC()
	st.PostCopyStarted = true
	st.Stage = model.AdaptiveStagePostCopy
	st.StageSince = &t
	return st
}

func postCopyStarted(status model.MachineMigrationStatus) bool {
	return status.Adaptive != nil && status.Adaptive.PostCopyStarted
}

// postCopySourceLostAfter is how long source status polls may keep failing
// after the post-copy switch before the transfer is treated as lost.
const postCopySourceLostAfter = 2 * time.Minute

// postCopySourceLost reports whether a failed source status poll means the
// post-copy transfer is gone: the adapter says the transfer or its VM no
// longer exists, or no telemetry has arrived for postCopySourceLostAfter.
// Before the switch a failing poll is only retried, since the source still
// holds the whole guest.
func postCopySourceLost(status model.MachineMigrationStatus, err error, now time.Time) bool {
	if !postCopyStarted(status) {
		return false
	}
	if migration.IsRejected(err) {
		return true
	}
	var last *time.Time
	if status.Progress != nil && status.Progress.UpdatedAt != nil {
		last = status.Progress.UpdatedAt
	} else {
		last = status.Adaptive.StageSince
	}
	return last != nil && now.Sub(*last) >= postCopySourceLostAfter
}

func adaptiveSample(transfer migration.TransferStatus) adaptive.Sample {
	s := adaptive.Sample{
		RAMTotal:       transfer.RAMTotal,
		RAMRemaining:   transfer.RAMRemaining,
		PostCopyActive: transfer.PostCopyActive,
	}
	if t := transfer.Telemetry; t != nil {
		s.DirtyRateMbps = t.DirtyRateMbps()
		s.ThroughputMbps = t.Mbps
		s.ExpectedDowntimeMs = t.ExpectedDowntimeMs
		s.CPUThrottlePct = t.CPUThrottlePct
	}
	return s
}

// transferProgress projects adapter telemetry into status.progress.
func transferProgress(transfer migration.TransferStatus, compressed bool, now time.Time) *model.MachineMigrationProgress {
	t := transfer.Telemetry
	if t == nil {
		return nil
	}
	ts := now.UTC()
	p := &model.MachineMigrationProgress{
		DirtyPagesRate:     t.DirtyPagesRate,
		DirtyRateMbps:      uint64(t.DirtyRateMbps()),
		ThroughputMbps:     uint64(t.Mbps),
		DuplicatePages:     t.DuplicatePages,
		DirtySyncCount:     t.DirtySyncCount,
		ExpectedDowntimeMs: t.ExpectedDowntimeMs,
		CPUThrottlePct:     t.CPUThrottlePct,
		PostcopyRequests:   t.PostcopyRequests,
		UpdatedAt:          &ts,
	}
	if compressed && t.NormalBytes > 0 && transfer.RAMTransferred > 0 {
		p.CompressionRatioPercent = t.NormalBytes * 100 / transfer.RAMTransferred
	}
	// The link drains remaining RAM at throughput minus what the guest
	// re-dirties; no estimate while the guest keeps up with the link.
	if net := t.Mbps - t.DirtyRateMbps(); net > 0 && transfer.RAMRemaining > 0 {
		p.EstimatedCompletionSeconds = uint64(float64(transfer.RAMRemaining*8) / (net * 1e6))
	}
	return p
}

// applyAdaptive runs the engine for one poll and applies its actions. It
// returns abort=true when the engine gave up on the transfer.
func (a *Agent) applyAdaptive(ctx context.Context, item model.MachineMigration, status *model.MachineMigrationStatus, session migration.Session, transfer migration.TransferStatus) (abort bool, reason string) {
	if item.Spec.Adaptive == nil || status.Adaptive == nil {
		return false, ""
	}
	caps, tuner := a.sourceCapabilities(ctx)
	settings := adaptiveSettings(item, status.Adaptive, caps, tuner)
	now := time.Now().UTC()
	next, actions := adaptive.Decide(settings, *status.Adaptive, adaptiveSample(transfer), now)
	for _, act := range actions {
		switch act.Kind {
		case adaptive.ActionTune:
			if _, err := tuner.Tune(ctx, session, status.TransferID, act.Tune); err != nil {
				a.Log.Warn("adaptive tune failed", "migration", item.Metadata.Name, "namespace", item.Namespace(), "error", err)
				adaptive.Record(&next, now, "tune-failed", err.Error())
			}
		case adaptive.ActionPostCopy:
			_, err := tuner.StartPostCopy(ctx, session, status.TransferID)
			switch {
			case err == nil:
				next.PostCopyStarted = true
			case migration.IsRejected(err):
				// Refused before QEMU switched: carry on in pre-copy with
				// post-copy off; the completion deadline still applies.
				adaptive.Record(&next, now, "postcopy-refused", err.Error())
				next.PostCopyArmed = false
				next.Stage = model.AdaptiveStageRelaxDowntime
			default:
				// The switch may have happened; never assume the source
				// can still be resumed.
				next.PostCopyStarted = true
				adaptive.Record(&next, now, "postcopy-uncertain", err.Error())
			}
		case adaptive.ActionAbort:
			abort, reason = true, act.Reason
		}
	}
	status.Adaptive = &next
	return abort, reason
}
