// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// Health actions healthDecision can return.
const (
	healthNone      = ""
	healthRestart   = "Restart"
	healthExhausted = "Exhausted"
	healthReset     = "Reset"
)

type healthDecision struct {
	Action string
	// Count is the restart count after this decision (Restart) or the
	// capped count (Exhausted).
	Count   int32
	Reason  string
	Message string
}

// reconcileMachineHealth applies spec.healthCheck.restartPolicy. kairon-node
// runs the probes and publishes status.health; this only reads that record
// and, when the policy is enabled and liveness is failing, asks kairon-node
// to power-cycle the Machine through the existing restart-requested
// annotation. It never restarts more than maxRestarts times (see
// healthRestartDecision) and records an Event for every action.
func (c *Controller) reconcileMachineHealth(ctx context.Context, machines []model.Machine, now time.Time) {
	for _, m := range machines {
		d := healthRestartDecision(m, now)
		if d.Action == healthNone {
			continue
		}
		if err := c.applyHealthDecision(ctx, m, d, now); err != nil {
			if !kube.IsNotFound(err) {
				c.Log.Error("health restart policy failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "action", d.Action, "error", err)
				if c.Metrics != nil {
					c.Metrics.ObserveReconcileItemError("health")
				}
			}
			continue
		}
		if d.Action == healthReset {
			continue
		}
		c.Log.Info("health restart policy", "namespace", m.Namespace(), "machine", m.Metadata.Name, "action", d.Action, "restarts", d.Count)
		evType := "Normal"
		if d.Action == healthExhausted {
			evType = "Warning"
		}
		if err := c.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, d.Reason, d.Message, evType); err != nil {
			c.Log.Debug("record health event failed", "machine", m.Metadata.Name, "error", err)
		}
	}
}

func (c *Controller) applyHealthDecision(ctx context.Context, m model.Machine, d healthDecision, now time.Time) error {
	ann := map[string]any{}
	switch d.Action {
	case healthRestart:
		ann[model.AnnotationRestartRequested] = now.UTC().Format(time.RFC3339Nano)
		ann[model.AnnotationHealthRestarts] = model.FormatHealthRestarts(d.Count, now)
	case healthExhausted:
		ann[model.AnnotationHealthExhausted] = "true"
	case healthReset:
		ann[model.AnnotationHealthRestarts] = nil
		ann[model.AnnotationHealthExhausted] = nil
	}
	return c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"annotations": ann}})
}

// healthRestartDecision is the pure core of the auto-restart policy: what,
// if anything, to do for m at now. It restarts only a Running Machine whose
// liveness probe is Failing, only after the backoff since the last restart
// has passed, and never once the restart count reaches maxRestarts; at the
// cap it returns Exhausted once (to record a Warning) and then nothing.
// Passing liveness for resetAfterSeconds since the last restart clears the
// counter.
func healthRestartDecision(m model.Machine, now time.Time) healthDecision {
	h := m.Spec.HealthCheck
	if h == nil || h.Liveness == nil || h.RestartPolicy == nil || !h.RestartPolicy.Enabled {
		return healthDecision{}
	}
	if m.Metadata.DeletionTimestamp != nil || m.Status.Phase != "Running" || m.Metadata.Annotations[model.AnnotationMigrationRef] != "" {
		return healthDecision{}
	}
	if ps := m.Spec.PowerState; ps != "" && ps != "Running" {
		return healthDecision{}
	}
	p := *h.RestartPolicy
	count, last := model.ParseHealthRestarts(m.Metadata.Annotations[model.AnnotationHealthRestarts])
	live := m.Status.Health
	if live == nil || live.Liveness == nil || live.Liveness.State != model.ProbeStateFailing {
		if count > 0 && live != nil && live.Liveness != nil && live.Liveness.State == model.ProbeStatePassing && !last.IsZero() && now.Sub(last) >= p.EffectiveResetAfter() {
			return healthDecision{Action: healthReset}
		}
		return healthDecision{}
	}
	// A restart is already requested and kairon-node has not acted on it.
	if v := m.Metadata.Annotations[model.AnnotationRestartRequested]; v != "" && v != m.Status.LastRestartRequest {
		return healthDecision{}
	}
	max := p.EffectiveMaxRestarts()
	if count >= max {
		if m.Metadata.Annotations[model.AnnotationHealthExhausted] == "true" {
			return healthDecision{}
		}
		return healthDecision{Action: healthExhausted, Count: count, Reason: "HealthRestartsExhausted",
			Message: fmt.Sprintf("liveness probe still failing after %d automatic restarts (spec.healthCheck.restartPolicy.maxRestarts); not restarting again. Delete the %s annotation to re-arm", count, model.AnnotationHealthRestarts)}
	}
	if count > 0 && now.Sub(last) < p.Backoff(count) {
		return healthDecision{}
	}
	return healthDecision{Action: healthRestart, Count: count + 1, Reason: "HealthRestart",
		Message: fmt.Sprintf("liveness probe failing (%s); restarting (%d of %d)", live.Liveness.Message, count+1, max)}
}
