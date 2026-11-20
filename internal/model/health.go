// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// ProbeTypeGuestAgent calls the guest agent (a qemu-guest-agent query);
	// it needs spec.guestAgent.enabled.
	ProbeTypeGuestAgent = "GuestAgent"
	// ProbeTypeTCP opens a TCP connection to status.guestIP:port.
	ProbeTypeTCP = "TCP"
	// ProbeTypeExec runs a command in the guest through the guest agent;
	// exit code 0 is healthy. It needs spec.guestAgent.enabled.
	ProbeTypeExec = "Exec"

	ProbeStateUnknown = "Unknown"
	ProbeStatePassing = "Passing"
	ProbeStateFailing = "Failing"

	DefaultProbePeriodSeconds    = 10
	DefaultProbeTimeoutSeconds   = 2
	DefaultProbeFailureThreshold = 3
	MaxProbeTimeoutSeconds       = 10

	DefaultHealthMaxRestarts       = 3
	DefaultHealthBackoffSeconds    = 60
	MinHealthBackoffSeconds        = 10
	MaxHealthBackoffSeconds        = 3600
	DefaultHealthResetAfterSeconds = 3600

	// AnnotationHealthRestarts is "COUNT@RFC3339": how many restarts the
	// spec.healthCheck.restartPolicy has requested, and when the last one
	// was. kairon-controller owns it; delete it to re-arm an exhausted
	// policy.
	AnnotationHealthRestarts = "kairon.zyvor.dev/health-restarts"
	// AnnotationHealthExhausted is set when the restart cap was reached, so
	// the Warning Event is recorded once.
	AnnotationHealthExhausted = "kairon.zyvor.dev/health-restarts-exhausted"
)

// HealthCheckSpec adds readiness and liveness probes to a Machine. Without
// it a Machine is Ready as soon as it is Running, as before.
type HealthCheckSpec struct {
	// Readiness decides the Ready condition and whether `kaironctl expose`
	// endpoints take traffic. Failing it never restarts the Machine.
	Readiness *ProbeSpec `json:"readiness,omitempty"`
	// Liveness detects a hung guest. Failing it marks the Machine not
	// Ready and, with restartPolicy.enabled, restarts it.
	Liveness *ProbeSpec `json:"liveness,omitempty"`
	// RestartPolicy opts into automatic restarts on a failing liveness probe.
	RestartPolicy *HealthRestartPolicy `json:"restartPolicy,omitempty"`
}

// ProbeSpec is one probe. Type picks the mechanism; kairon-node runs it.
type ProbeSpec struct {
	// Type is GuestAgent, TCP or Exec.
	Type string `json:"type"`
	// Port is the TCP port on status.guestIP (TCP only).
	Port int32 `json:"port,omitempty"`
	// Command is the argv run in the guest (Exec only); no shell is used.
	Command []string `json:"command,omitempty"`
	// InitialDelaySeconds waits this long after the Machine starts Running
	// before probing.
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`
	// PeriodSeconds is the interval between probes (default 10).
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`
	// TimeoutSeconds bounds one probe (default 2, at most 10).
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// FailureThreshold is the consecutive failures that make the probe
	// Failing (default 3).
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
	// SuccessThreshold is the consecutive successes that make it Passing
	// (default 1; always 1 for liveness).
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// HealthRestartPolicy bounds automatic restarts. Restarts back off
// exponentially and stop at MaxRestarts; the Machine is then left alone
// (not Ready) until the counter resets or an operator intervenes.
type HealthRestartPolicy struct {
	Enabled bool `json:"enabled,omitempty"`
	// MaxRestarts is the cap (default 3).
	MaxRestarts int32 `json:"maxRestarts,omitempty"`
	// BackoffSeconds is the wait before the first retry (default 60); it
	// doubles per restart, up to 3600.
	BackoffSeconds int64 `json:"backoffSeconds,omitempty"`
	// ResetAfterSeconds clears the restart counter after the liveness probe
	// has passed this long since the last restart (default 3600).
	ResetAfterSeconds int64 `json:"resetAfterSeconds,omitempty"`
}

// HealthStatus is kairon-node's probe record. Readiness and Liveness
// carry no omitempty so a merge patch can clear them.
type HealthStatus struct {
	Readiness *ProbeStatus `json:"readiness"`
	Liveness  *ProbeStatus `json:"liveness"`
	// RunningSince (RFC3339) is when probing started counting for the
	// current run; initialDelaySeconds is measured from it.
	RunningSince string `json:"runningSince,omitempty"`
	// RestartMarker is the status.lastRestartRequest the probes last saw;
	// a change resets them.
	RestartMarker string `json:"restartMarker,omitempty"`
}

// ProbeStatus is the outcome of one probe.
type ProbeStatus struct {
	// State is Unknown, Passing or Failing.
	State                string `json:"state"`
	ConsecutiveFailures  int32  `json:"consecutiveFailures,omitempty"`
	ConsecutiveSuccesses int32  `json:"consecutiveSuccesses,omitempty"`
	// LastTransitionTime (RFC3339) is when State last changed.
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	// Message is the last failure, empty while passing.
	Message string `json:"message,omitempty"`
}

func (p ProbeSpec) EffectivePeriod() time.Duration {
	return secondsOr(p.PeriodSeconds, DefaultProbePeriodSeconds)
}

func (p ProbeSpec) EffectiveTimeout() time.Duration {
	return secondsOr(p.TimeoutSeconds, DefaultProbeTimeoutSeconds)
}

func (p ProbeSpec) EffectiveFailureThreshold() int32 {
	if p.FailureThreshold <= 0 {
		return DefaultProbeFailureThreshold
	}
	return p.FailureThreshold
}

func (p ProbeSpec) EffectiveSuccessThreshold() int32 {
	if p.SuccessThreshold <= 0 {
		return 1
	}
	return p.SuccessThreshold
}

func secondsOr(n int32, def int32) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}

func (r HealthRestartPolicy) EffectiveMaxRestarts() int32 {
	if r.MaxRestarts <= 0 {
		return DefaultHealthMaxRestarts
	}
	return r.MaxRestarts
}

// Backoff is the wait after the count-th restart (count >= 1): the base
// doubled per earlier restart, capped at MaxHealthBackoffSeconds.
func (r HealthRestartPolicy) Backoff(count int32) time.Duration {
	base := r.BackoffSeconds
	if base <= 0 {
		base = DefaultHealthBackoffSeconds
	}
	d := time.Duration(base) * time.Second
	limit := MaxHealthBackoffSeconds * time.Second
	for i := int32(1); i < count && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (r HealthRestartPolicy) EffectiveResetAfter() time.Duration {
	if r.ResetAfterSeconds <= 0 {
		return DefaultHealthResetAfterSeconds * time.Second
	}
	return time.Duration(r.ResetAfterSeconds) * time.Second
}

// ParseHealthRestarts reads AnnotationHealthRestarts ("COUNT@RFC3339").
// A missing or malformed value is zero restarts.
func ParseHealthRestarts(v string) (count int32, last time.Time) {
	n, ts, ok := strings.Cut(strings.TrimSpace(v), "@")
	if !ok {
		return 0, time.Time{}
	}
	c, err := strconv.Atoi(n)
	t, terr := time.Parse(time.RFC3339, ts)
	if err != nil || terr != nil || c < 0 {
		return 0, time.Time{}
	}
	return int32(c), t
}

// FormatHealthRestarts is the inverse of ParseHealthRestarts.
func FormatHealthRestarts(count int32, last time.Time) string {
	return fmt.Sprintf("%d@%s", count, last.UTC().Format(time.RFC3339))
}

// MachineReady reports whether the Machine should receive traffic: it is
// Running and its Ready condition is not False. A Machine with no Ready
// condition yet (older kairon-node) is treated as before: Running is Ready.
func MachineReady(m Machine) bool {
	if m.Status.Phase != "Running" {
		return false
	}
	c, ok := FindCondition(m.Status.Conditions, "Ready")
	return !ok || c.Status != "False"
}

// ValidateHealthCheck rejects probes kairon-node cannot run.
func ValidateHealthCheck(spec MachineSpec) error {
	h := spec.HealthCheck
	if h == nil {
		return nil
	}
	if h.Readiness != nil {
		if err := validateProbe("spec.healthCheck.readiness", *h.Readiness, spec.GuestAgent.Enabled); err != nil {
			return err
		}
	}
	if h.Liveness != nil {
		if err := validateProbe("spec.healthCheck.liveness", *h.Liveness, spec.GuestAgent.Enabled); err != nil {
			return err
		}
	}
	if r := h.RestartPolicy; r != nil {
		if r.Enabled && h.Liveness == nil {
			return fmt.Errorf("spec.healthCheck.restartPolicy needs spec.healthCheck.liveness")
		}
		if r.MaxRestarts < 0 || r.ResetAfterSeconds < 0 {
			return fmt.Errorf("spec.healthCheck.restartPolicy: values must not be negative")
		}
		if r.BackoffSeconds != 0 && (r.BackoffSeconds < MinHealthBackoffSeconds || r.BackoffSeconds > MaxHealthBackoffSeconds) {
			return fmt.Errorf("spec.healthCheck.restartPolicy.backoffSeconds must be %d to %d", MinHealthBackoffSeconds, MaxHealthBackoffSeconds)
		}
	}
	return nil
}

func validateProbe(path string, p ProbeSpec, guestAgent bool) error {
	switch p.Type {
	case ProbeTypeGuestAgent, ProbeTypeExec:
		if !guestAgent {
			return fmt.Errorf("%s: type %s needs spec.guestAgent.enabled", path, p.Type)
		}
	case ProbeTypeTCP:
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("%s.port: TCP probes need a port from 1 to 65535", path)
		}
	default:
		return fmt.Errorf("%s.type %q: want GuestAgent, TCP or Exec", path, p.Type)
	}
	if p.Type == ProbeTypeExec && len(p.Command) == 0 {
		return fmt.Errorf("%s.command: Exec probes need a command", path)
	}
	if p.Type != ProbeTypeExec && len(p.Command) > 0 {
		return fmt.Errorf("%s.command: only Exec probes take a command", path)
	}
	if p.Type != ProbeTypeTCP && p.Port != 0 {
		return fmt.Errorf("%s.port: only TCP probes take a port", path)
	}
	if p.InitialDelaySeconds < 0 || p.PeriodSeconds < 0 || p.TimeoutSeconds < 0 || p.FailureThreshold < 0 || p.SuccessThreshold < 0 {
		return fmt.Errorf("%s: values must not be negative", path)
	}
	if p.TimeoutSeconds > MaxProbeTimeoutSeconds {
		return fmt.Errorf("%s.timeoutSeconds must be at most %d", path, MaxProbeTimeoutSeconds)
	}
	return nil
}
