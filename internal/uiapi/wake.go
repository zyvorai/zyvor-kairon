// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// wakePollInterval is how often ensureAwake re-reads a waking Machine.
var wakePollInterval = 500 * time.Millisecond

var errWakeTimeout = errors.New("machine did not wake in time")

// activityTouches remembers when kairon-ui last wrote
// kairon.zyvor.dev/last-activity per Machine, so a busy relay costs one
// API write per model.ActivityWriteInterval rather than one per request.
var activityTouches sync.Map

// ensureAwake resumes a Machine kairon-controller suspended for idleness
// when its spec.lifecycle.wakeOnConnect is set, and waits up to
// wakeTimeoutSeconds for it to be Running again. A Machine an operator
// parked by hand (no kairon.zyvor.dev/idle-suspended) stays as it is;
// the caller's own "not Running" check then reports it. It also records
// relay activity so an open connection counts as use. woke reports a
// resume from a cold state (anything but Paused), after which the guest
// agent may still be booting.
func (s *Server) ensureAwake(ctx context.Context, m model.Machine) (out model.Machine, woke bool, err error) {
	l := m.Spec.Lifecycle
	if l == nil {
		return m, false, nil
	}
	if m.Status.Phase == "Running" {
		s.touchActivity(ctx, m)
		return m, false, nil
	}
	if !l.WakeOnConnect || m.Metadata.Annotations[model.AnnotationIdleSuspended] == "" {
		return m, false, nil
	}
	from := m.Status.Phase
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			model.AnnotationIdleSuspended: nil,
			model.AnnotationLastActivity:  time.Now().UTC().Format(time.RFC3339),
		}},
		"spec": map[string]any{"powerState": "Running"},
	}
	if err := s.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		return m, false, fmt.Errorf("wake machine: %w", err)
	}
	activityTouches.Store(m.Namespace()+"/"+m.Metadata.Name, time.Now())
	if s.Log != nil {
		s.Log.Info("uiapi waking idle machine", "namespace", m.Namespace(), "name", m.Metadata.Name, "from", from)
	}
	ctx, cancel := context.WithTimeout(ctx, l.WakeTimeout())
	defer cancel()
	for {
		cur, err := s.Kube.GetMachine(ctx, m.Namespace(), m.Metadata.Name)
		if err == nil && cur.Status.Phase == "Running" && cur.Status.RuntimeID != "" && cur.Status.NodeName != "" {
			return cur, from != "Paused", nil
		}
		select {
		case <-ctx.Done():
			return m, false, fmt.Errorf("%w (still %s after %s)", errWakeTimeout, from, l.WakeTimeout())
		case <-time.After(wakePollInterval):
		}
	}
}

// writeWakeError reports an ensureAwake failure.
func writeWakeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errWakeTimeout) {
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	}
	writeUpstreamError(w, err)
}

// touchActivity stamps kairon.zyvor.dev/last-activity on a Machine with an
// idle timeout, at most once per model.ActivityWriteInterval.
func (s *Server) touchActivity(ctx context.Context, m model.Machine) {
	if m.Spec.Lifecycle == nil || m.Spec.Lifecycle.IdleTimeoutSeconds <= 0 {
		return
	}
	key := m.Namespace() + "/" + m.Metadata.Name
	now := time.Now()
	if v, ok := activityTouches.Load(key); ok && now.Sub(v.(time.Time)) < model.ActivityWriteInterval {
		return
	}
	activityTouches.Store(key, now)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		model.AnnotationLastActivity: now.UTC().Format(time.RFC3339),
	}}}
	if err := s.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil && s.Log != nil {
		s.Log.Debug("uiapi touch activity failed", "namespace", m.Namespace(), "name", m.Metadata.Name, "error", err)
	}
}

// keepActive touches activity for as long as a long-lived relay is open,
// so an attached but quiet session is not suspended under the user. The
// returned func stops it.
func (s *Server) keepActive(m model.Machine) (stop func()) {
	if m.Spec.Lifecycle == nil || m.Spec.Lifecycle.IdleTimeoutSeconds <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		t := time.NewTicker(model.ActivityWriteInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.touchActivity(ctx, m)
			}
		}
	}()
	return cancel
}

// guestExecAllowed is the identity half of the guest exec/file/proxy
// gates: admins always; other operators only with SandboxOperatorExec and
// a Machine labelled model.LabelSandbox=true. consoleAuthorized still
// applies on top.
func (s *Server) guestExecAllowed(ctx context.Context, m model.Machine, username string) bool {
	if s.isAdminIdentity(ctx, username) {
		return true
	}
	return s.SandboxOperatorExec && username != "" && m.Metadata.Labels[model.LabelSandbox] == "true"
}

// waitForGuestAgent polls the guest agent after a cold wake until it
// answers, so the first exec after a resume does not fail on a guest that
// is still booting. It gives up quietly at the deadline and lets the real
// request report whatever is wrong.
func (s *Server) waitForGuestAgent(ctx context.Context, nodeAddr string, m model.Machine) {
	ctx, cancel := context.WithTimeout(ctx, m.Spec.Lifecycle.WakeTimeout())
	defer cancel()
	for {
		var out fsfreezeStatusResponse
		if err := s.relayToNodeGet(ctx, nodeAddr, "qga-fsfreeze-status/"+m.Status.RuntimeID, &out); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
