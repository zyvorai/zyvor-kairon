// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileGuestFreeze serves kaironctl freeze/unfreeze through
// AnnotationFsFreezeUntil: freeze while the deadline is ahead, thaw (and
// drop the request) once it has passed or the request is removed. A
// snapshot quiesce in progress owns the freeze state, so this waits for it;
// a quiesce thaw that undid an operator freeze is re-frozen on the next
// tick. Failures are logged and retried, like reconcileGuestQuiesce.
func (a *Agent) reconcileGuestFreeze(ctx context.Context, m model.Machine, rec *fluxvm.Record, now time.Time) {
	if !m.Spec.GuestAgent.Enabled || m.Metadata.Annotations[model.AnnotationQuiesceRequest] != "" || m.Metadata.Annotations[model.AnnotationQuiesceStatus] != "" {
		return
	}
	raw, requested := m.Metadata.Annotations[model.AnnotationFsFreezeUntil]
	frozen := m.Metadata.Annotations[model.AnnotationFsFrozen] != ""
	until, err := time.Parse(time.RFC3339, raw)
	active := requested && err == nil && now.Before(until)

	switch {
	case active:
		if frozen {
			if st, err := a.Flux.QGAFsfreezeStatus(ctx, rec.ID()); err != nil || st == "frozen" {
				return
			}
		}
		if _, err := a.Flux.QGAFsfreezeFreeze(ctx, rec.ID()); err != nil {
			a.Log.Warn("guest freeze failed, will retry", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			return
		}
		if !frozen {
			a.patchAnnotations(ctx, m, map[string]any{model.AnnotationFsFrozen: now.UTC().Format(time.RFC3339)})
			a.recordEvent(ctx, m, "FilesystemsFrozen", "guest filesystems frozen until "+raw, "Normal")
		}
	case requested || frozen:
		if _, err := a.Flux.QGAFsfreezeThaw(ctx, rec.ID()); err != nil {
			a.Log.Warn("guest thaw failed, will retry", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			return
		}
		a.patchAnnotations(ctx, m, map[string]any{model.AnnotationFsFrozen: nil, model.AnnotationFsFreezeUntil: nil})
		msg := "guest filesystems thawed"
		if requested {
			msg += " (unfreeze timeout " + raw + " reached)"
		}
		a.recordEvent(ctx, m, "FilesystemsThawed", msg, "Normal")
	}
}

func (a *Agent) patchAnnotations(ctx context.Context, m model.Machine, annotations map[string]any) {
	if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"annotations": annotations}}); err != nil {
		a.Log.Error("patch machine annotations failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}

func (a *Agent) recordEvent(ctx context.Context, m model.Machine, reason, msg, kind string) {
	if err := a.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, reason, msg, kind); err != nil {
		a.Log.Debug("record event failed", "machine", m.Metadata.Name, "reason", reason, "error", err)
	}
}
