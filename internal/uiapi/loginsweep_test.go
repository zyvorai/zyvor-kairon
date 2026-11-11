// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"strconv"
	"testing"
	"time"
)

func TestLoginAttemptsAreEvicted(t *testing.T) {
	s := &Server{}
	for i := 0; i < loginSweepEvery*3; i++ {
		_ = s.loginState("user-" + strconv.Itoa(i))
	}
	// Everything just touched is "idle" relative to a point far in the future.
	s.sweepLoginAttempts(time.Now().Add(time.Hour))
	n := 0
	s.loginAttempts.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("expected idle, unlocked entries to be evicted, %d remain", n)
	}
	// A locked entry survives the sweep.
	st := s.loginState("locked")
	st.mu.Lock()
	st.lockedUntil = time.Now().Add(time.Hour)
	st.mu.Unlock()
	s.sweepLoginAttempts(time.Now().Add(30 * time.Minute))
	if _, ok := s.loginAttempts.Load("locked"); !ok {
		t.Fatal("a currently locked entry must not be evicted")
	}
}

// A failure recorded on a state the sweep is concurrently evicting must not be
// lost: after the sweep, the count lives in the map entry.
func TestLoginSweepDoesNotDropFailureCount(t *testing.T) {
	s := &Server{}
	ctx := t.Context()
	st := s.loginState("u")
	// Simulate a goroutine that obtained the state before the sweep ran.
	s.sweepLoginAttempts(time.Now().Add(time.Hour))
	st.mu.Lock()
	evicted := st.evicted
	st.mu.Unlock()
	if !evicted {
		t.Fatal("swept state must be marked evicted")
	}
	s.recordLoginResult(ctx, "u", false)
	cur := s.loginState("u")
	cur.mu.Lock()
	defer cur.mu.Unlock()
	if cur.count != 1 {
		t.Fatalf("failure count lost: got %d, want 1", cur.count)
	}
}
