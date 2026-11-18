// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeGuestAgent is a scriptable GuestAgentClient.
type fakeGuestAgent struct {
	pingErr  error
	execCode int64
	execErr  error
	pings    int
	execs    [][]string
}

func (f *fakeGuestAgent) GuestPing(context.Context, string) error {
	f.pings++
	return f.pingErr
}

func (f *fakeGuestAgent) GuestExec(_ context.Context, _ string, argv []string, _ time.Duration) (int64, error) {
	f.execs = append(f.execs, argv)
	return f.execCode, f.execErr
}

func TestEvalProbeThresholds(t *testing.T) {
	now := time.Now()
	spec := model.ProbeSpec{FailureThreshold: 2, SuccessThreshold: 2}
	var st *model.ProbeStatus
	steps := []struct {
		err  error
		want string
	}{
		{nil, model.ProbeStateUnknown},
		{nil, model.ProbeStatePassing},
		{errors.New("boom"), model.ProbeStatePassing},
		{errors.New("boom"), model.ProbeStateFailing},
		{errors.New("boom"), model.ProbeStateFailing},
		{nil, model.ProbeStateFailing},
		{nil, model.ProbeStatePassing},
	}
	for i, s := range steps {
		st = evalProbe(spec, st, s.err, now)
		if st.State != s.want {
			t.Fatalf("step %d: state %s, want %s (%+v)", i, st.State, s.want, st)
		}
	}
	if st.Message != "" || st.ConsecutiveFailures != 0 {
		t.Fatalf("passing probe kept failure data: %+v", st)
	}
	// Counters stop at the threshold: steady failure changes nothing.
	f := evalProbe(spec, nil, errors.New("x"), now)
	f = evalProbe(spec, f, errors.New("x"), now)
	again := evalProbe(spec, f, errors.New("x"), now.Add(time.Hour))
	if *again != *f {
		t.Fatalf("steady failing status changed: %+v vs %+v", again, f)
	}
}

func TestProbeDue(t *testing.T) {
	now := time.Now()
	spec := model.ProbeSpec{InitialDelaySeconds: 30, PeriodSeconds: 10}
	if probeDue(spec, now.Add(-10*time.Second), time.Time{}, now) {
		t.Error("due inside the initial delay")
	}
	if !probeDue(spec, now.Add(-40*time.Second), time.Time{}, now) {
		t.Error("first probe after the delay not due")
	}
	if probeDue(spec, now.Add(-40*time.Second), now.Add(-5*time.Second), now) {
		t.Error("due before the period")
	}
	if !probeDue(spec, now.Add(-40*time.Second), now.Add(-10*time.Second), now) {
		t.Error("not due after the period")
	}
}

func TestProberRun(t *testing.T) {
	g := &fakeGuestAgent{}
	var dialed string
	p := prober{guest: g, dial: func(_ context.Context, addr string) error { dialed = addr; return nil }}
	ctx := context.Background()
	if err := p.run(ctx, model.ProbeSpec{Type: model.ProbeTypeGuestAgent}, "vm", ""); err != nil || g.pings != 1 {
		t.Fatalf("ping: %v %d", err, g.pings)
	}
	g.pingErr = errors.New("agent down")
	if err := p.run(ctx, model.ProbeSpec{Type: model.ProbeTypeGuestAgent}, "vm", ""); err == nil {
		t.Fatal("failed ping passed")
	}
	ex := model.ProbeSpec{Type: model.ProbeTypeExec, Command: []string{"/bin/check", "-q"}}
	if err := p.run(ctx, ex, "vm", ""); err != nil || len(g.execs) != 1 || g.execs[0][0] != "/bin/check" {
		t.Fatalf("exec: %v %v", err, g.execs)
	}
	g.execCode = 3
	if err := p.run(ctx, ex, "vm", ""); err == nil {
		t.Fatal("non-zero exit passed")
	}
	g.execCode, g.execErr = 0, errors.New("exec failed")
	if err := p.run(ctx, ex, "vm", ""); err == nil {
		t.Fatal("exec error passed")
	}
	tcp := model.ProbeSpec{Type: model.ProbeTypeTCP, Port: 8080}
	if err := p.run(ctx, tcp, "vm", "10.0.0.5"); err != nil || dialed != "10.0.0.5:8080" {
		t.Fatalf("tcp: %v %q", err, dialed)
	}
	if err := p.run(ctx, tcp, "vm", ""); err == nil {
		t.Fatal("tcp probe without a guest IP passed")
	}
}

func TestTCPDialRealListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	addr := l.Addr().String()
	if err := tcpDial(context.Background(), addr); err != nil {
		t.Fatalf("open port: %v", err)
	}
	_ = l.Close()
	if err := tcpDial(context.Background(), addr); err == nil {
		t.Fatal("closed port passed")
	}
}

func TestObserveHealthReadinessAndRestartReset(t *testing.T) {
	g := &fakeGuestAgent{}
	a := &Agent{GuestAgentClient: g}
	rec := &fluxvm.Record{IDValue: "vm1"}
	m := model.Machine{Spec: model.MachineSpec{
		GuestAgent: model.GuestAgentSpec{Enabled: true},
		HealthCheck: &model.HealthCheckSpec{
			Readiness: &model.ProbeSpec{Type: model.ProbeTypeGuestAgent, PeriodSeconds: 1},
			Liveness:  &model.ProbeSpec{Type: model.ProbeTypeGuestAgent, PeriodSeconds: 1, FailureThreshold: 2},
		},
	}}
	now := time.Now()
	st := model.MachineStatus{Phase: "Running", GuestIP: "10.0.0.2"}

	// Passing: Ready.
	ready, reason, _ := a.observeHealth(context.Background(), m, rec, &st, now)
	if !ready || reason != "FluxVMReconciled" || st.Health.Readiness.State != model.ProbeStatePassing {
		t.Fatalf("first tick: %v %s %+v", ready, reason, st.Health)
	}
	// Not yet due: nothing is probed again.
	pings := g.pings
	a.observeHealth(context.Background(), m, rec, &st, now.Add(200*time.Millisecond))
	if g.pings != pings {
		t.Fatal("probed before the period elapsed")
	}
	// Two failures flip liveness to Failing and the Machine to not Ready.
	g.pingErr = errors.New("no answer")
	tick := now
	var reason2 string
	for i := 0; i < 2; i++ {
		tick = tick.Add(2 * time.Second)
		ready, reason2, _ = a.observeHealth(context.Background(), m, rec, &st, tick)
	}
	if ready || reason2 != "LivenessProbeFailed" || st.Health.Liveness.State != model.ProbeStateFailing {
		t.Fatalf("after failures: %v %s %+v", ready, reason2, st.Health.Liveness)
	}
	// A restart (new lastRestartRequest) resets the probes to Unknown/pending.
	g.pingErr = nil
	st.LastRestartRequest = "r1"
	tick = tick.Add(2 * time.Second)
	m2 := m
	m2.Spec.HealthCheck = &model.HealthCheckSpec{Readiness: &model.ProbeSpec{Type: model.ProbeTypeGuestAgent, InitialDelaySeconds: 60}}
	ready, reason, _ = a.observeHealth(context.Background(), m2, rec, &st, tick)
	if ready || reason != "ReadinessProbePending" || st.Health.Liveness != nil {
		t.Fatalf("after restart: %v %s %+v", ready, reason, st.Health)
	}
	// Leaving Running clears the record.
	st.Phase = "Stopped"
	if ready, _, _ = a.observeHealth(context.Background(), m, rec, &st, tick); ready || st.Health.Readiness != nil {
		t.Fatalf("stopped: %v %+v", ready, st.Health)
	}
}

func TestObserveHealthWithoutSpec(t *testing.T) {
	a := &Agent{GuestAgentClient: &fakeGuestAgent{}}
	st := model.MachineStatus{Phase: "Running"}
	if ready, _, _ := a.observeHealth(context.Background(), model.Machine{}, &fluxvm.Record{IDValue: "v"}, &st, time.Now()); !ready || st.Health != nil {
		t.Fatalf("no spec: %v %+v", ready, st.Health)
	}
	st.Health = &model.HealthStatus{Liveness: &model.ProbeStatus{State: model.ProbeStateFailing}}
	a.observeHealth(context.Background(), model.Machine{}, &fluxvm.Record{IDValue: "v"}, &st, time.Now())
	if st.Health == nil || st.Health.Liveness != nil {
		t.Fatalf("stale health not cleared: %+v", st.Health)
	}
}
