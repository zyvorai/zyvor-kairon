// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"testing"
	"time"
)

func TestValidateHealthCheck(t *testing.T) {
	ga := GuestAgentSpec{Enabled: true}
	ok := []MachineSpec{
		{},
		{HealthCheck: &HealthCheckSpec{Readiness: &ProbeSpec{Type: ProbeTypeTCP, Port: 80}}},
		{GuestAgent: ga, HealthCheck: &HealthCheckSpec{
			Liveness:      &ProbeSpec{Type: ProbeTypeGuestAgent, PeriodSeconds: 5},
			Readiness:     &ProbeSpec{Type: ProbeTypeExec, Command: []string{"/bin/true"}},
			RestartPolicy: &HealthRestartPolicy{Enabled: true, MaxRestarts: 2, BackoffSeconds: 30},
		}},
	}
	for i, s := range ok {
		if err := ValidateHealthCheck(s); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := map[string]MachineSpec{
		"no type":          {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{}}},
		"tcp no port":      {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeTCP}}},
		"ga needs agent":   {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeGuestAgent}}},
		"exec no command":  {GuestAgent: ga, HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeExec}}},
		"command on tcp":   {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeTCP, Port: 1, Command: []string{"x"}}}},
		"port on ga":       {GuestAgent: ga, HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeGuestAgent, Port: 1}}},
		"negative":         {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeTCP, Port: 1, PeriodSeconds: -1}}},
		"timeout too long": {HealthCheck: &HealthCheckSpec{Liveness: &ProbeSpec{Type: ProbeTypeTCP, Port: 1, TimeoutSeconds: 11}}},
		"restart needs liveness": {HealthCheck: &HealthCheckSpec{
			Readiness: &ProbeSpec{Type: ProbeTypeTCP, Port: 1}, RestartPolicy: &HealthRestartPolicy{Enabled: true}}},
		"backoff too small": {HealthCheck: &HealthCheckSpec{
			Liveness: &ProbeSpec{Type: ProbeTypeTCP, Port: 1}, RestartPolicy: &HealthRestartPolicy{BackoffSeconds: 1}}},
	}
	for name, s := range bad {
		if err := ValidateHealthCheck(s); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestHealthRestartPolicyBackoff(t *testing.T) {
	p := HealthRestartPolicy{BackoffSeconds: 60}
	want := []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second}
	for i, w := range want {
		if got := p.Backoff(int32(i + 1)); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := p.Backoff(40); got != time.Hour {
		t.Errorf("Backoff(40) = %s, want 1h cap", got)
	}
	if got := (HealthRestartPolicy{}).Backoff(1); got != time.Minute {
		t.Errorf("default backoff = %s", got)
	}
}

func TestHealthRestartsRoundTrip(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c, last := ParseHealthRestarts(FormatHealthRestarts(2, now))
	if c != 2 || !last.Equal(now) {
		t.Fatalf("got %d %s", c, last)
	}
	for _, v := range []string{"", "x", "2", "-1@2026-01-02T03:04:05Z", "2@nope"} {
		if c, _ := ParseHealthRestarts(v); c != 0 {
			t.Errorf("%q parsed to %d", v, c)
		}
	}
}

func TestMachineReady(t *testing.T) {
	cond := func(s string) []Condition { return []Condition{{Type: "Ready", Status: s}} }
	cases := []struct {
		name string
		st   MachineStatus
		want bool
	}{
		{"running no condition", MachineStatus{Phase: "Running"}, true},
		{"running ready", MachineStatus{Phase: "Running", Conditions: cond("True")}, true},
		{"running not ready", MachineStatus{Phase: "Running", Conditions: cond("False")}, false},
		{"stopped", MachineStatus{Phase: "Stopped", Conditions: cond("True")}, false},
	}
	for _, c := range cases {
		if got := MachineReady(Machine{Status: c.st}); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
