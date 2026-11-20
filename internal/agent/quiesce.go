// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileGuestQuiesce is kairon-node's own half of the guest quiesce
// protocol kairon-controller's MachineSnapshot reconcile drives (see
// internal/controller/snapshot.go's own doc comment for why this is an
// annotation-based request/response pair rather than a direct RPC --
// kairon-controller has no network path to this Machine's FluxVM
// instance, only the node it's scheduled on does). Best-effort: every
// failure here is logged and retried on a later reconcile tick, never
// returned as a hard error that would fail the rest of reconcileMachine,
// the same soft-fail posture guest-IP resolution already has.
//
// AnnotationQuiesceRequest present, not yet matched by
// AnnotationQuiesceStatus -> freeze. AnnotationQuiesceRequest absent
// while AnnotationQuiesceStatus is still present -> thaw. Thaw is
// retried indefinitely on failure (no giveup path) -- see
// internal/controller/snapshot.go's awaitGuestThaw for why a stuck-frozen
// guest filesystem must never be silently abandoned.
func (a *Agent) reconcileGuestQuiesce(ctx context.Context, m model.Machine, rec *fluxvm.Record) {
	if !m.Spec.GuestAgent.Enabled {
		return
	}
	requestName, _, requestSet := model.ParseQuiesceRef(m.Metadata.Annotations[model.AnnotationQuiesceRequest])
	statusName, _, statusSet := model.ParseQuiesceRef(m.Metadata.Annotations[model.AnnotationQuiesceStatus])

	switch {
	case requestSet && (!statusSet || statusName != requestName):
		if _, err := a.Flux.QGAFsfreezeFreeze(ctx, rec.ID()); err != nil {
			a.Log.Warn("guest quiesce freeze failed, will retry", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			return
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
			model.AnnotationQuiesceStatus: m.Metadata.Annotations[model.AnnotationQuiesceRequest],
		}}}
		if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
			a.Log.Error("guest quiesce: patch frozen confirmation failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	case !requestSet && statusSet:
		if _, err := a.Flux.QGAFsfreezeThaw(ctx, rec.ID()); err != nil {
			a.Log.Warn("guest quiesce thaw failed, will retry", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			return
		}
		clear := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationQuiesceStatus: nil}}}
		if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, clear); err != nil {
			a.Log.Error("guest quiesce: clear frozen confirmation failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	}
}
