// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"testing"
	"time"
)

func TestValidateLifecycle(t *testing.T) {
	ok := []*LifecycleSpec{
		nil,
		{},
		{MaxLifetimeSeconds: 3600},
		{IdleTimeoutSeconds: 60, IdleAction: IdleActionHibernate, WakeOnConnect: true},
	}
	for _, l := range ok {
		if err := ValidateLifecycle(l); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	bad := []*LifecycleSpec{
		{IdleTimeoutSeconds: 10},
		{IdleAction: "Sleep"},
		{MaxLifetimeSeconds: -1},
	}
	for _, l := range bad {
		if err := ValidateLifecycle(l); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}

func TestLifecycleDefaults(t *testing.T) {
	var l LifecycleSpec
	if l.IdlePowerState() != "Paused" || l.EffectiveIdleCPUMillicores() != DefaultIdleCPUMillicores || l.WakeTimeout().Seconds() != DefaultWakeTimeoutSeconds {
		t.Fatalf("defaults: %q %d %v", l.IdlePowerState(), l.EffectiveIdleCPUMillicores(), l.WakeTimeout())
	}
	for action, state := range map[string]string{IdleActionHibernate: "Hibernated", IdleActionHalt: "Halted", IdleActionStop: "Stopped", IdleActionDelete: ""} {
		if got := (LifecycleSpec{IdleAction: action}).IdlePowerState(); got != state {
			t.Errorf("%s -> %q, want %q", action, got, state)
		}
	}
}

func TestAccrueClaimUsage(t *testing.T) {
	bound := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	u, a, s := AccrueClaimUsage(ClaimUsage{}, bound, "Running", 2, 4, bound.Add(10*time.Second))
	if a != 0 || s != 0 || u.ActiveSeconds != 0 {
		t.Fatalf("accrued before the interval: %+v", u)
	}
	u, a, _ = AccrueClaimUsage(u, bound, "Running", 2, 4, bound.Add(90500*time.Millisecond))
	if a != 90 || u.ActiveSeconds != 90 || u.VCPUSeconds != 180 || u.MemoryGiBSeconds != 360 || u.LastAccrued != "2026-10-14T12:01:30Z" {
		t.Fatalf("running: %+v", u)
	}
	u, _, s = AccrueClaimUsage(u, bound, "Paused", 2, 4, bound.Add(150*time.Second))
	if s != 60 || u.SuspendedSeconds != 60 || u.ActiveSeconds != 90 || u.VCPUSeconds != 180 {
		t.Fatalf("paused: %+v", u)
	}
	u, a, s = AccrueClaimUsage(u, bound, "Migrating", 2, 4, bound.Add(200*time.Second))
	if a != 0 || s != 0 || u.LastAccrued != "2026-10-14T12:03:20Z" {
		t.Fatalf("other phase should advance without accruing: %+v", u)
	}
}
