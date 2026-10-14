// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileLifecycle enforces spec.lifecycle: it deletes a Machine past
// maxLifetimeSeconds, and applies idleAction to a Running Machine with no
// guest or relay activity for idleTimeoutSeconds. Activity comes from
// kairon-node (status.lifecycle.lastActivityTime) and kairon-ui
// (kairon.zyvor.dev/last-activity). The Machine opted in, so disruption
// budgets do not hold these actions back; a Machine that is migrating or
// being deleted is left alone.
func (c *Controller) reconcileLifecycle(ctx context.Context, machines []model.Machine, now time.Time) {
	for _, m := range machines {
		l := m.Spec.Lifecycle
		if l == nil || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		action, reason, msg := lifecycleDecision(m, now)
		if action == "" {
			continue
		}
		if err := c.applyLifecycleAction(ctx, m, action, now); err != nil {
			if !kube.IsNotFound(err) {
				c.Log.Error("lifecycle action failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "action", action, "error", err)
				if c.Metrics != nil {
					c.Metrics.ObserveReconcileItemError("lifecycle")
				}
			}
			continue
		}
		c.Log.Info("lifecycle action applied", "namespace", m.Namespace(), "machine", m.Metadata.Name, "action", action, "reason", reason)
		if c.Metrics != nil {
			c.Metrics.ObserveLifecycleAction(action)
		}
		if err := c.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, reason, msg, "Normal"); err != nil {
			c.Log.Debug("record lifecycle event failed", "machine", m.Metadata.Name, "error", err)
		}
	}
}

// lifecycleExpire is the action name for maxLifetimeSeconds deletions.
const lifecycleExpire = "Expire"

// lifecycleDecision returns the action due for m now (an IdleAction value
// or lifecycleExpire), the Event reason and message; action "" means none.
func lifecycleDecision(m model.Machine, now time.Time) (action, reason, msg string) {
	l := m.Spec.Lifecycle
	// A warm pool member is the pool's, not a user's, until a claim binds it.
	if m.Metadata.Labels[model.LabelPoolState] == model.PoolStateWarm {
		return "", "", ""
	}
	start := m.Metadata.CreationTimestamp
	if t, err := time.Parse(time.RFC3339, m.Metadata.Annotations[model.AnnotationLifecycleStart]); err == nil {
		start = t
	}
	if l.MaxLifetimeSeconds > 0 && !start.IsZero() {
		max := time.Duration(l.MaxLifetimeSeconds) * time.Second
		if now.Sub(start) >= max {
			return lifecycleExpire, "LifetimeExpired", fmt.Sprintf("deleted after spec.lifecycle.maxLifetimeSeconds (%s)", max)
		}
	}
	if l.IdleTimeoutSeconds <= 0 || m.Status.Phase != "Running" || m.Metadata.Annotations[model.AnnotationMigrationRef] != "" {
		return "", "", ""
	}
	if ps := m.Spec.PowerState; ps != "" && ps != "Running" {
		return "", "", ""
	}
	last := lastActivity(m)
	if last.IsZero() {
		return "", "", ""
	}
	timeout := time.Duration(l.IdleTimeoutSeconds) * time.Second
	idle := now.Sub(last)
	if idle < timeout {
		return "", "", ""
	}
	return l.EffectiveIdleAction(), "IdleSuspended", fmt.Sprintf("no activity for %s (idleTimeoutSeconds %d); applied idleAction %s", idle.Round(time.Second), l.IdleTimeoutSeconds, l.EffectiveIdleAction())
}

// lastActivity is the later of kairon-node's and kairon-ui's activity
// records; zero when neither has reported yet.
func lastActivity(m model.Machine) time.Time {
	var last time.Time
	if m.Status.Lifecycle != nil {
		if t, err := time.Parse(time.RFC3339, m.Status.Lifecycle.LastActivityTime); err == nil {
			last = t
		}
	}
	if t, err := time.Parse(time.RFC3339, m.Metadata.Annotations[model.AnnotationLastActivity]); err == nil && t.After(last) {
		last = t
	}
	return last
}

func (c *Controller) applyLifecycleAction(ctx context.Context, m model.Machine, action string, now time.Time) error {
	if action == lifecycleExpire || action == model.IdleActionDelete {
		return c.Kube.DeleteMachine(ctx, m.Namespace(), m.Metadata.Name)
	}
	return c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{model.AnnotationIdleSuspended: now.UTC().Format(time.RFC3339)}},
		"spec":     map[string]any{"powerState": m.Spec.Lifecycle.IdlePowerState()},
	})
}
