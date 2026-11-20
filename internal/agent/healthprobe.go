// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// GuestAgentClient is the slice of the FluxVM client the probes use, so
// tests can substitute a fake.
type GuestAgentClient interface {
	// GuestPing succeeds when the guest agent answers.
	GuestPing(ctx context.Context, id string) error
	// GuestExec runs argv in the guest and returns its exit code.
	GuestExec(ctx context.Context, id string, argv []string, timeout time.Duration) (int64, error)
}

// fluxGuestAgent adapts the FluxVM client. The ping is a real
// qemu-guest-agent round trip (guest-network-get-interfaces): FluxVM has
// no dedicated ping endpoint, and this call fails when the agent is not
// running or not answering.
type fluxGuestAgent struct{ c *fluxvm.Client }

func (f fluxGuestAgent) GuestPing(ctx context.Context, id string) error {
	_, err := f.c.QGANetworkInterfaces(ctx, id)
	return err
}

func (f fluxGuestAgent) GuestExec(ctx context.Context, id string, argv []string, timeout time.Duration) (int64, error) {
	t := uint64(timeout / time.Second)
	if t == 0 {
		t = 1
	}
	res, err := f.c.QGAExec(ctx, id, fluxvm.QGAExecRequest{Path: argv[0], Args: argv[1:], TimeoutSeconds: &t})
	if err != nil {
		return 0, err
	}
	return res.ExitCode, nil
}

// prober runs one probe and reports whether it passed.
type prober struct {
	guest GuestAgentClient
	dial  func(ctx context.Context, addr string) error
}

func tcpDial(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// run executes spec against the runtime id (guestIP for TCP); the error is
// the failure reason, nil means the probe passed.
func (p prober) run(ctx context.Context, spec model.ProbeSpec, id, guestIP string) error {
	ctx, cancel := context.WithTimeout(ctx, spec.EffectiveTimeout())
	defer cancel()
	switch spec.Type {
	case model.ProbeTypeGuestAgent:
		return p.guest.GuestPing(ctx, id)
	case model.ProbeTypeExec:
		if len(spec.Command) == 0 {
			return fmt.Errorf("exec probe has no command")
		}
		code, err := p.guest.GuestExec(ctx, id, spec.Command, spec.EffectiveTimeout())
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("command exited with code %d", code)
		}
		return nil
	case model.ProbeTypeTCP:
		if guestIP == "" {
			return fmt.Errorf("no guest IP yet")
		}
		return p.dial(ctx, net.JoinHostPort(guestIP, strconv.Itoa(int(spec.Port))))
	}
	return fmt.Errorf("unknown probe type %q", spec.Type)
}

// evalProbe folds one result into the previous probe status. It is pure.
// Counters stop at their threshold so a steady state writes nothing.
func evalProbe(spec model.ProbeSpec, prev *model.ProbeStatus, probeErr error, now time.Time) *model.ProbeStatus {
	cur := model.ProbeStatus{State: model.ProbeStateUnknown}
	if prev != nil {
		cur = *prev
	}
	failT, okT := spec.EffectiveFailureThreshold(), spec.EffectiveSuccessThreshold()
	old := cur.State
	if probeErr == nil {
		cur.ConsecutiveFailures = 0
		cur.Message = ""
		cur.ConsecutiveSuccesses = min(cur.ConsecutiveSuccesses+1, okT)
		if cur.ConsecutiveSuccesses >= okT {
			cur.State = model.ProbeStatePassing
		}
	} else {
		cur.ConsecutiveSuccesses = 0
		cur.Message = probeErr.Error()
		cur.ConsecutiveFailures = min(cur.ConsecutiveFailures+1, failT)
		if cur.ConsecutiveFailures >= failT {
			cur.State = model.ProbeStateFailing
		}
	}
	if cur.State != old || cur.LastTransitionTime == "" {
		cur.LastTransitionTime = now.UTC().Format(time.RFC3339)
	}
	return &cur
}

// probeDue reports whether a probe should run now. last is the previous
// run (zero when none this process lifetime).
func probeDue(spec model.ProbeSpec, runningSince, last, now time.Time) bool {
	if now.Before(runningSince.Add(time.Duration(spec.InitialDelaySeconds) * time.Second)) {
		return false
	}
	return last.IsZero() || now.Sub(last) >= spec.EffectivePeriod()
}

// healthReady is the Ready condition status for a Running Machine.
func healthReady(h *model.HealthStatus, spec *model.HealthCheckSpec) (ready bool, reason, msg string) {
	if spec == nil {
		return true, "FluxVMReconciled", ""
	}
	if spec.Liveness != nil && h != nil && h.Liveness != nil && h.Liveness.State == model.ProbeStateFailing {
		return false, "LivenessProbeFailed", h.Liveness.Message
	}
	if spec.Readiness != nil {
		if h == nil || h.Readiness == nil || h.Readiness.State == model.ProbeStateUnknown {
			return false, "ReadinessProbePending", "waiting for the readiness probe to pass"
		}
		if h.Readiness.State == model.ProbeStateFailing {
			return false, "ReadinessProbeFailed", h.Readiness.Message
		}
	}
	return true, "FluxVMReconciled", ""
}

// healthLast remembers when each runtime was last probed; a restart of
// kairon-node just probes once straight away.
type healthLast struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (h *healthLast) get(k string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[k]
}

func (h *healthLast) set(k string, t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]time.Time{}
	}
	h.m[k] = t
}

// observeHealth runs the due probes for a spec.healthCheck Machine and
// records them in status.health. It returns the Ready condition to
// publish. Probes run only while Running; leaving Running, or a restart,
// resets them. Probing never blocks longer than the probe timeouts.
func (a *Agent) observeHealth(ctx context.Context, m model.Machine, rec *fluxvm.Record, status *model.MachineStatus, now time.Time) (ready bool, reason, msg string) {
	spec := m.Spec.HealthCheck
	if spec == nil || (spec.Readiness == nil && spec.Liveness == nil) {
		if status.Health != nil {
			status.Health = &model.HealthStatus{}
		}
		return status.Phase == "Running", "FluxVMReconciled", ""
	}
	if status.Phase != "Running" {
		status.Health = &model.HealthStatus{}
		return false, "NotRunning", ""
	}
	h := model.HealthStatus{}
	if status.Health != nil {
		h = *status.Health
	}
	if h.RunningSince == "" || h.RestartMarker != status.LastRestartRequest {
		h = model.HealthStatus{RunningSince: now.UTC().Format(time.RFC3339), RestartMarker: status.LastRestartRequest}
	}
	since, _ := time.Parse(time.RFC3339, h.RunningSince)
	p := prober{guest: a.guestAgent(), dial: tcpDial}
	if spec.Readiness == nil {
		h.Readiness = nil
	} else {
		h.Readiness = a.runProbe(ctx, p, "readiness", *spec.Readiness, h.Readiness, since, rec, status.GuestIP, now)
	}
	if spec.Liveness == nil {
		h.Liveness = nil
	} else {
		lv := *spec.Liveness
		lv.SuccessThreshold = 1
		h.Liveness = a.runProbe(ctx, p, "liveness", lv, h.Liveness, since, rec, status.GuestIP, now)
	}
	status.Health = &h
	return healthReady(&h, spec)
}

func (a *Agent) runProbe(ctx context.Context, p prober, kind string, spec model.ProbeSpec, prev *model.ProbeStatus, since time.Time, rec *fluxvm.Record, guestIP string, now time.Time) *model.ProbeStatus {
	key := rec.ID() + "/" + kind
	if !probeDue(spec, since, a.healthProbed.get(key), now) {
		return prev
	}
	a.healthProbed.set(key, now)
	return evalProbe(spec, prev, p.run(ctx, spec, rec.ID(), guestIP), now)
}

func (a *Agent) guestAgent() GuestAgentClient {
	if a.GuestAgentClient != nil {
		return a.GuestAgentClient
	}
	return fluxGuestAgent{a.Flux}
}
