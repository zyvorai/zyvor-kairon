// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestCapacityDeniesUnknownKeysWithoutResettingBuckets(t *testing.T) {
	l := NewWithCapacity(1, 1, 2)
	now := time.Unix(100, 0)
	l.now = func() time.Time { return now }
	l.Allow("a")
	l.Allow("b")
	for i := 0; i < 1000; i++ {
		if allowed, retry := l.Allow(fmt.Sprint(i)); allowed || retry <= 0 {
			t.Fatalf("unknown key admitted at capacity: %v, %v", allowed, retry)
		}
	}
	if len(l.buckets) != 2 {
		t.Fatalf("tracked %d keys, want 2", len(l.buckets))
	}
	if allowed, _ := l.Allow("a"); allowed {
		t.Fatal("churn reset an exhausted bucket")
	}
	now = now.Add(time.Second)
	if allowed, _ := l.Allow("a"); !allowed {
		t.Fatal("existing key must still refill at capacity")
	}
	l.Prune(500 * time.Millisecond)
	if allowed, _ := l.Allow("c"); !allowed {
		t.Fatal("pruning must free capacity for a new key")
	}
}

func BenchmarkLimiterAllow(b *testing.B) {
	l := New(1e12, 1000000)
	l.Allow("client")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow("client")
		}
	})
}
