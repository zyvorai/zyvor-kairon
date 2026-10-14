// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// maxStopGracePeriod bounds AnnotationStopGracePeriod so a typo cannot keep
// a Machine Running for days after powerState was set to Stopped.
const maxStopGracePeriod = 10 * time.Minute

// restartRequest is the kairon.zyvor.dev/restart-requested value kairon-node
// has not acted on yet, or "".
func restartRequest(m model.Machine) string {
	v := strings.TrimSpace(m.Metadata.Annotations[model.AnnotationRestartRequested])
	if v == "" || v == m.Status.LastRestartRequest {
		return ""
	}
	return v
}

// stopGracePeriod is how long a stop waits for the guest to power itself
// off; 0 without a guest agent or the annotation.
func stopGracePeriod(m model.Machine) time.Duration {
	if !m.Spec.GuestAgent.Enabled {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(m.Metadata.Annotations[model.AnnotationStopGracePeriod]))
	if err != nil || n <= 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, maxStopGracePeriod)
}

// awaitGuestShutdown runs before ensureStopped/ensureHalted power a running
// runtime off. With a grace period it asks the guest agent to power the
// guest off once, records the deadline in status.shutdownDeadline, and
// reports wait=true until the guest has stopped or the deadline passes.
// It never blocks the reconcile loop.
func (a *Agent) awaitGuestShutdown(ctx context.Context, m model.Machine, rec *fluxvm.Record, now time.Time) (wait bool, err error) {
	grace := stopGracePeriod(m)
	if grace == 0 || rec == nil || rec.ID() == "" || normalizePhase(rec.Status) != "Running" {
		return false, nil
	}
	if m.Status.ShutdownDeadline == "" {
		a.requestGuestPoweroff(ctx, m, rec.ID())
		status := m.Status
		status.ShutdownDeadline = now.Add(grace).UTC().Format(time.RFC3339)
		status.Message = fmt.Sprintf("asked the guest to power off; stopping the runtime in at most %s", grace)
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "ShuttingDown", Message: status.Message,
		})
		return true, a.patchMachineStatusIfChanged(ctx, m, status)
	}
	deadline, err := time.Parse(time.RFC3339, m.Status.ShutdownDeadline)
	if err != nil || !now.Before(deadline) {
		return false, nil
	}
	return true, nil
}

// requestGuestPoweroff asks the guest to shut down through the guest agent:
// systemd/sysvinit first, PowerShell for a Windows guest. Best effort; the
// grace deadline still applies when the guest ignores it.
func (a *Agent) requestGuestPoweroff(ctx context.Context, m model.Machine, id string) {
	t := uint64(10)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Detached and delayed: a synchronous poweroff kills the guest agent
	// before it reports the exec result, which reads as a failure.
	_, err := a.Flux.QGAExec(cctx, id, fluxvm.QGAExecRequest{Path: "/bin/sh", Args: []string{"-c", "(sleep 2; systemctl poweroff || poweroff || shutdown -h now) </dev/null >/dev/null 2>&1 &"}, TimeoutSeconds: &t})
	if err != nil {
		_, err = a.Flux.QGAExec(cctx, id, fluxvm.QGAExecRequest{Powershell: "Stop-Computer -Force", TimeoutSeconds: &t})
	}
	if err != nil {
		a.log().Warn("guest poweroff request failed; stopping at the grace deadline", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		return
	}
	a.log().Info("asked the guest to power off", "namespace", m.Namespace(), "machine", m.Metadata.Name)
}
