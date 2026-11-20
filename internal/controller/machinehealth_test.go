// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func healthMachine(count int32, last time.Time, state string) model.Machine {
	m := model.Machine{}
	m.Metadata.Annotations = map[string]string{}
	m.Spec.HealthCheck = &model.HealthCheckSpec{
		Liveness:      &model.ProbeSpec{Type: model.ProbeTypeTCP, Port: 22},
		RestartPolicy: &model.HealthRestartPolicy{Enabled: true, MaxRestarts: 3, BackoffSeconds: 60, ResetAfterSeconds: 600},
	}
	m.Status.Phase = "Running"
	m.Status.Health = &model.HealthStatus{Liveness: &model.ProbeStatus{State: state, Message: "connection refused"}}
	if count > 0 {
		m.Metadata.Annotations[model.AnnotationHealthRestarts] = model.FormatHealthRestarts(count, last)
	}
	return m
}

func TestHealthRestartDecision(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	failing := model.ProbeStateFailing
	t.Run("first failure restarts", func(t *testing.T) {
		d := healthRestartDecision(healthMachine(0, time.Time{}, failing), now)
		if d.Action != healthRestart || d.Count != 1 || d.Reason != "HealthRestart" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("backoff doubles", func(t *testing.T) {
		// After restart 2 the wait is 120s.
		if d := healthRestartDecision(healthMachine(2, now.Add(-100*time.Second), failing), now); d.Action != healthNone {
			t.Fatalf("restarted inside the backoff: %+v", d)
		}
		if d := healthRestartDecision(healthMachine(2, now.Add(-121*time.Second), failing), now); d.Action != healthRestart || d.Count != 3 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("cap reached never restarts, warns once", func(t *testing.T) {
		m := healthMachine(3, now.Add(-24*time.Hour), failing)
		d := healthRestartDecision(m, now)
		if d.Action != healthExhausted || d.Count != 3 {
			t.Fatalf("%+v", d)
		}
		m.Metadata.Annotations[model.AnnotationHealthExhausted] = "true"
		for i := 0; i < 5; i++ {
			if d := healthRestartDecision(m, now.Add(time.Duration(i)*time.Hour)); d.Action != healthNone {
				t.Fatalf("restart loop after the cap: %+v", d)
			}
		}
	})
	t.Run("passing is left alone", func(t *testing.T) {
		if d := healthRestartDecision(healthMachine(1, now.Add(-time.Minute), model.ProbeStatePassing), now); d.Action != healthNone {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("healthy long enough resets the counter", func(t *testing.T) {
		d := healthRestartDecision(healthMachine(2, now.Add(-11*time.Minute), model.ProbeStatePassing), now)
		if d.Action != healthReset {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("unknown liveness does nothing", func(t *testing.T) {
		if d := healthRestartDecision(healthMachine(0, time.Time{}, model.ProbeStateUnknown), now); d.Action != healthNone {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("restart already pending", func(t *testing.T) {
		m := healthMachine(0, time.Time{}, failing)
		m.Metadata.Annotations[model.AnnotationRestartRequested] = "t1"
		if d := healthRestartDecision(m, now); d.Action != healthNone {
			t.Fatalf("%+v", d)
		}
		m.Status.LastRestartRequest = "t1"
		if d := healthRestartDecision(m, now); d.Action != healthRestart {
			t.Fatalf("handled request should not block: %+v", d)
		}
	})
	t.Run("opt-in only and only while Running", func(t *testing.T) {
		m := healthMachine(0, time.Time{}, failing)
		m.Spec.HealthCheck.RestartPolicy.Enabled = false
		if d := healthRestartDecision(m, now); d.Action != healthNone {
			t.Fatalf("policy off: %+v", d)
		}
		m = healthMachine(0, time.Time{}, failing)
		m.Spec.HealthCheck.RestartPolicy = nil
		if d := healthRestartDecision(m, now); d.Action != healthNone {
			t.Fatalf("no policy: %+v", d)
		}
		for _, mut := range []func(*model.Machine){
			func(m *model.Machine) { m.Status.Phase = "Stopped" },
			func(m *model.Machine) { m.Spec.PowerState = "Paused" },
			func(m *model.Machine) { m.Metadata.Annotations[model.AnnotationMigrationRef] = "mig" },
			func(m *model.Machine) { t := now; m.Metadata.DeletionTimestamp = &t },
		} {
			m := healthMachine(0, time.Time{}, failing)
			mut(&m)
			if d := healthRestartDecision(m, now); d.Action != healthNone {
				t.Fatalf("acted on a machine that should be left alone: %+v", d)
			}
		}
	})
}
