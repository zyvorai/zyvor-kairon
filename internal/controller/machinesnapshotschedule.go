// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileMachineSnapshotSchedules lists every MachineSnapshotSchedule and,
// for each one Due (or manually triggered -- see below), creates a
// MachineSnapshot for every Machine matching its Selector -- mirrors
// reconcileDisruptionBudgetsStatus's own "list all, compute, patch status"
// shape (disruption.go), plus the actual object-creation side-effect a pure
// status reconciler doesn't have. Tolerant of the CRD not being installed,
// like every other optional CRD kind in this reconcile loop. A create
// failure for one Machine is logged and counted
// (kairon_reconcile_item_errors_total{kind="snapshotschedule"}), never fails
// the whole tick or skips the rest of that schedule's matches.
//
// A Due schedule whose window is already past spec.startingDeadlineSeconds
// (Spec.DeadlineExceeded) is a third, distinct outcome from "fired" or "not
// due": no MachineSnapshot is created at all this tick, but status is still
// patched -- lastRunTime advances to now (so the next tick starts counting
// a fresh interval rather than re-detecting the same missed window forever),
// lastRunSnapshotCount is 0, and lastRunError names the skip. This is the
// opt-in behavior; startingDeadlineSeconds unset (0, the default) never
// takes this branch, preserving the original always-fire-once-due behavior
// for every existing schedule.
//
// A schedule carrying an unhandled AnnotationSnapshotScheduleTriggerNow
// request (model.TriggerNowRequested) is a fourth outcome, checked first,
// ahead of Due/DeadlineExceeded entirely: it fires exactly like a normal due
// run (same per-Machine create loop, same spec.keepLast pruning), but
// bypasses BOTH spec.suspend and spec.startingDeadlineSeconds -- "run this
// right now" is never "too late", and a paused schedule can still be asked
// for one snapshot without permanently unpausing it first. It cannot fire
// twice for the same request: the handled annotation value is copied into
// status.lastHandledTriggerTime in the same status patch, so an unchanged
// annotation is no longer an outstanding request on the next tick. A
// schedule that's both manually triggered and separately Due this same tick
// still only fires once, not twice.
func (c *Controller) reconcileMachineSnapshotSchedules(ctx context.Context, machines []model.Machine) error {
	schedules, err := c.Kube.ListMachineSnapshotSchedules(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return nil
		}
		return err
	}
	now := time.Now()
	for _, sched := range schedules {
		triggerRequest := sched.Metadata.Annotations[model.AnnotationSnapshotScheduleTriggerNow]
		manual := model.TriggerNowRequested(triggerRequest, sched.Status.LastHandledTriggerTime)
		if err := sched.Spec.Validate(); err != nil {
			msg := "invalid spec: " + err.Error()
			if sched.Status.LastRunError != msg {
				c.Log.Warn("machine snapshot schedule skipped", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "error", err)
				if statusErr := c.Kube.SetMachineSnapshotScheduleError(ctx, sched.Namespace(), sched.Metadata.Name, msg); statusErr != nil {
					c.Log.Error("machine snapshot schedule status patch failed", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "error", statusErr)
				}
			}
			continue
		}
		if !manual && !sched.Due(sched.Status.LastRunTime, now) {
			continue
		}
		if !manual && sched.DeadlineExceeded(sched.Status.LastRunTime, now) {
			c.Log.Warn("scheduled snapshot run skipped: starting deadline exceeded", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "startingDeadlineSeconds", sched.Spec.StartingDeadlineSeconds)
			status := model.MachineSnapshotScheduleStatus{
				LastRunTime:          now,
				LastRunSnapshotCount: 0,
				LastRunError:         fmt.Sprintf("skipped: this run was more than startingDeadlineSeconds (%ds) late", sched.Spec.StartingDeadlineSeconds),
				NextRunTime:          sched.NextRunAfter(now),
			}
			if statusErr := c.Kube.PatchMachineSnapshotScheduleStatus(ctx, sched.Namespace(), sched.Metadata.Name, status); statusErr != nil {
				c.Log.Error("machine snapshot schedule status patch failed", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "error", statusErr)
			}
			continue
		}
		if manual {
			c.Log.Info("scheduled snapshot run manually triggered", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name)
		}
		count := 0
		var firstErr error
		for _, m := range machines {
			if m.Namespace() != sched.Namespace() || !model.LabelsMatch(m.Metadata.Labels, sched.Spec.Selector) {
				continue
			}
			snap := model.MachineSnapshot{
				TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshot},
				Metadata: model.ObjectMeta{
					// Includes m.Metadata.Name, not just the schedule name
					// and timestamp: `now` is computed once per reconcile
					// tick above, so a schedule whose Selector matches more
					// than one Machine would otherwise generate the exact
					// same name for every match this tick. A real apiserver
					// enforces per-namespace name uniqueness, so only the
					// first Machine's MachineSnapshot would actually get
					// created -- every other match's create call would fail
					// with 409 AlreadyExists, silently dropping snapshots
					// for every Machine after the first one on every fire of
					// any schedule matching more than one Machine. The fake
					// httptest servers this reconciler's own unit tests run
					// against don't enforce that uniqueness (see
					// TestReconcileMachineSnapshotSchedulesDueCreatesUniqueNamesPerMachine's
					// doc comment), so this went uncaught until traced
					// through by hand.
					Name:      fmt.Sprintf("%s-%s-%d", sched.Metadata.Name, m.Metadata.Name, now.Unix()),
					Namespace: sched.Namespace(),
					Labels:    map[string]string{model.SnapshotScheduleLabel: sched.Metadata.Name},
				},
				Spec: model.MachineSnapshotSpec{
					MachineName:             m.Metadata.Name,
					VolumeSnapshotClassName: sched.Spec.VolumeSnapshotClassName,
				},
			}
			if _, err := c.Kube.CreateMachineSnapshot(ctx, sched.Namespace(), snap); err != nil {
				c.Log.Error("scheduled snapshot create failed", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "machine", m.Metadata.Name, "error", err)
				if c.Metrics != nil {
					c.Metrics.ObserveReconcileItemError("snapshotschedule")
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			count++
			if sched.Spec.KeepLast > 0 || sched.Spec.MaxAgeSeconds > 0 {
				c.pruneScheduledSnapshots(ctx, sched.Namespace(), sched.Metadata.Name, m.Metadata.Name, sched.Spec.KeepLast, sched.Spec.MaxAgeSeconds, now)
			}
		}
		status := model.MachineSnapshotScheduleStatus{LastRunTime: now, LastRunSnapshotCount: count, NextRunTime: sched.NextRunAfter(now)}
		if firstErr != nil {
			status.LastRunError = firstErr.Error()
		}
		if manual {
			// Recorded in the SAME patch as the run it satisfies -- never a
			// separate write -- so a crash between "fired" and "marked
			// handled" can't happen: either both landed, or neither did (in
			// which case the next tick just sees the same unhandled request
			// and correctly retries).
			status.LastHandledTriggerTime = triggerRequest
		}
		if statusErr := c.Kube.PatchMachineSnapshotScheduleStatus(ctx, sched.Namespace(), sched.Metadata.Name, status); statusErr != nil {
			c.Log.Error("machine snapshot schedule status patch failed", "namespace", sched.Namespace(), "schedule", sched.Metadata.Name, "error", statusErr)
		}
	}
	return nil
}

// pruneScheduledSnapshots deletes the oldest ready-to-use MachineSnapshots
// this exact schedule created for this exact Machine (identified by
// model.SnapshotScheduleLabel, never a manually-created or
// different-schedule-created one) once there are more than keepLast of
// them, or (maxAgeSeconds) older than that -- see
// model.ScheduledSnapshotsToPrune; the newest is always kept. Only status.readyToUse snapshots are counted or deleted -- a
// snapshot still Freezing/Thawing/Pending never counts toward the limit and
// is never itself a deletion candidate, so a still-in-progress snapshot can
// never be the thing that gets pruned, and an old-but-still-only-ready
// snapshot is never deleted out from under a not-yet-ready replacement.
// Best-effort: a list or delete failure is logged, counted against the
// existing snapshotschedule reconcile-item-error metric, and left for the
// next due tick to retry -- never fails the schedule's own status patch.
func (c *Controller) pruneScheduledSnapshots(ctx context.Context, ns, scheduleName, machineName string, keepLast, maxAgeSeconds int, now time.Time) {
	all, err := c.Kube.ListMachineSnapshotsNamespace(ctx, ns)
	if err != nil {
		c.Log.Error("listing snapshots for schedule pruning failed", "namespace", ns, "schedule", scheduleName, "machine", machineName, "error", err)
		if c.Metrics != nil {
			c.Metrics.ObserveReconcileItemError("snapshotschedule")
		}
		return
	}
	var mine []model.MachineSnapshot
	for _, s := range all {
		if s.Metadata.Labels[model.SnapshotScheduleLabel] == scheduleName && s.Spec.MachineName == machineName && s.Status.ReadyToUse {
			mine = append(mine, s)
		}
	}
	for _, old := range model.ScheduledSnapshotsToPrune(mine, keepLast, maxAgeSeconds, now) {
		if err := c.Kube.DeleteMachineSnapshot(ctx, ns, old.Metadata.Name); err != nil {
			c.Log.Error("scheduled snapshot prune delete failed", "namespace", ns, "schedule", scheduleName, "machine", machineName, "snapshot", old.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("snapshotschedule")
			}
		}
	}
}
