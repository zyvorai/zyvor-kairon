// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdTrigger(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 2 {
		fatal(fmt.Errorf("usage: kaironctl trigger snapshotschedule NAME [-n NAMESPACE]"))
	}
	kind := strings.ToLower(args[0])
	name := args[1]
	ns, _ := nsFlag(args[2:])
	switch kind {
	case "snapshotschedule", "snapshotschedules", "machinesnapshotschedules":
		cmdTriggerSnapshotSchedule(ctx, kc, ns, name)
	default:
		fatal(fmt.Errorf("trigger only supports snapshotschedule, got %q", kind))
	}
}

// cmdTriggerSnapshotSchedule requests an immediate, out-of-band run of a
// MachineSnapshotSchedule -- "back up these Machines right now" without
// waiting for spec.intervalSeconds to elapse, and without the awkward
// workarounds an operator had to reach for before this existed (temporarily
// shrinking intervalSeconds, or flipping spec.suspend off and back on).
//
// Implemented as the same durable-annotation-request pattern
// AnnotationQuiesceRequest/AnnotationCordonEvacuateAttemptedAt already
// establish for "ask the controller to do a thing on its next reconcile
// tick, and remember it was asked" -- not a bespoke RPC or subresource of
// its own. This just merge-patches
// model.AnnotationSnapshotScheduleTriggerNow to the current RFC3339
// timestamp (touching no other annotation the schedule already carries);
// reconcileMachineSnapshotSchedules notices any value that doesn't match
// status.lastHandledTriggerTime yet as an unhandled request on its very
// next tick, fires exactly like a normal due run (including
// spec.keepLast pruning), and records the timestamp it handled into
// status.lastHandledTriggerTime so the same request is never re-fired on a
// later tick.
//
// Bypasses BOTH spec.suspend and spec.startingDeadlineSeconds on the
// controller side (see reconcileMachineSnapshotSchedules) -- a paused
// schedule can still be asked for one snapshot right now without
// permanently unpausing it first, and "too late" has no meaning for a
// request that's asking to run at this exact instant. It does NOT bypass
// spec.selector: a schedule matching zero Machines right now still counts
// as triggered (status advances exactly like a normal zero-match tick), it
// just creates nothing.
//
// Firing this way still advances status.lastRunTime/nextRunTime exactly
// like any other fire -- the schedule's normal interval countdown restarts
// from this manual run, it doesn't layer a "bonus" run on top of the
// pre-existing schedule.
func cmdTriggerSnapshotSchedule(ctx context.Context, kc *kube.Client, ns, name string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationSnapshotScheduleTriggerNow: ts}}}
	if err := kc.PatchMachineSnapshotSchedule(ctx, ns, name, patch); err != nil {
		fatal(fmt.Errorf("patch machinesnapshotschedule %s/%s: %w", ns, name, err))
	}
	okf("snapshotschedule/%s: manual run requested; kairon-controller will snapshot every matching Machine on its next reconcile tick, regardless of spec.suspend or the normal interval", name)
}

// machineSpecFromFlags registers the Machine-spec-shaped flags shared by a
// plain `create` (one Machine) and `create machineset` (every replica's
// template) onto fs, returning a closure that builds the resulting
// model.MachineSpec once fs.Parse has run, plus the --image flag's own
// pointer so each caller can enforce its own "image is required" check
// after parsing (both do; a MachineSet with no image would never actually
// boot). Extracted so the two callers can never drift apart on how a flag
// maps onto MachineSpec -- anything this doesn't cover (placement, device
// claims, security, per-volume claims) needs kubectl apply/YAML for either
// caller, same limit `create` already had before `create machineset`
