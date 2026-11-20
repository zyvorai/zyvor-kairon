// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestAllowPermitsUpToBurstThenThrottles(t *testing.T) {
	l := New(1, 3) // 1 req/s, burst 3
	for i := range 3 {
		if allowed, _ := l.Allow("a"); !allowed {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	allowed, retryAfter := l.Allow("a")
	if allowed {
		t.Fatal("4th request should be throttled, burst exhausted")
	}
	if retryAfter <= 0 {
		t.Fatalf("retryAfter = %v, want > 0", retryAfter)
	}
}

func TestAllowRefillsOverTime(t *testing.T) {
	l := New(10, 1) // 10 req/s, burst 1
	start := time.Now()
	l.now = func() time.Time { return start }
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("first request should be allowed")
	}
	if allowed, _ := l.Allow("a"); allowed {
		t.Fatal("second immediate request should be throttled")
	}
	// 100ms at 10 req/s refills exactly 1 token.
	l.now = func() time.Time { return start.Add(100 * time.Millisecond) }
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("request after refill window should be allowed")
	}
}

func TestAllowTracksKeysIndependently(t *testing.T) {
	l := New(1, 1)
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("a's first request should be allowed")
	}
	if allowed, _ := l.Allow("a"); allowed {
		t.Fatal("a's second immediate request should be throttled")
	}
	if allowed, _ := l.Allow("b"); !allowed {
		t.Fatal("b's first request should be allowed even though a is throttled -- independent buckets")
	}
}

func TestPruneRemovesOnlyStaleBuckets(t *testing.T) {
	l := New(1, 1)
	start := time.Now()
	l.now = func() time.Time { return start }
	l.Allow("stale")
	l.now = func() time.Time { return start.Add(time.Hour) }
	l.Allow("fresh")

	l.now = func() time.Time { return start.Add(time.Hour) }
	l.Prune(30 * time.Minute)

	l.mu.Lock()
	_, staleStillThere := l.buckets["stale"]
	_, freshStillThere := l.buckets["fresh"]
	l.mu.Unlock()
	if staleStillThere {
		t.Error("stale bucket should have been pruned")
	}
	if !freshStillThere {
		t.Error("fresh bucket should not have been pruned")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	l := New(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, time.Millisecond, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancellation")
	}
}
