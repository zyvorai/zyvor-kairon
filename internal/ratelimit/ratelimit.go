// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package ratelimit is a small, hand-rolled per-key token-bucket rate
// limiter -- stdlib-only, so wiring it into kairon-ui doesn't need a third
// exception to this project's Go-stdlib-only design guarantee (after
// OIDC and CSI) for what a basic token bucket does in well under a
// hundred lines. See internal/uiapi's withRateLimit for the one place
// this is used today: bounding request volume per remote address across
// kairon-ui's whole HTTP surface, beyond the per-username login lockout
// (internal/uiapi/auth.go) that already existed for repeated failed
// passwords specifically.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// DefaultMaxKeys bounds state even during bursts of previously unseen IPs.
const DefaultMaxKeys = 65536

// Limiter tracks one token bucket per key. Not a global var by design --
// callers construct one and pass it explicitly, matching this codebase's
// existing style (metrics.Recorder, Agent.MigrationPeer, etc.).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rps     float64
	burst   float64
	now     func() time.Time
	maxKeys int
}

// New builds a Limiter allowing, per key, an average of rps requests per
// second with bursts up to burst requests before throttling kicks in. A
// key's bucket starts full (burst tokens). Previously unseen keys are
// denied when DefaultMaxKeys is reached until Prune frees capacity.
func New(rps float64, burst int) *Limiter {
	return NewWithCapacity(rps, burst, DefaultMaxKeys)
}

// NewWithCapacity sets a hard bound on tracked keys. At capacity, unknown
// keys are denied until Prune frees space; existing buckets are never evicted
// to admit new keys, which would let churn reset an exhausted bucket.
// Nonpositive capacity uses DefaultMaxKeys.
func NewWithCapacity(rps float64, burst, maxKeys int) *Limiter {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &Limiter{
		buckets: map[string]*bucket{},
		rps:     rps,
		burst:   float64(burst),
		now:     time.Now,
		maxKeys: maxKeys,
	}
}

// Allow reports whether a request identified by key is allowed right now,
// consuming one token from key's bucket if so. When it returns false,
// retryAfter is how long until key's bucket would have at least one token
// again -- round up before using it as a Retry-After header value.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			return false, time.Second
		}
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.lastSeen).Seconds()
		b.tokens += elapsed * l.rps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.lastSeen = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	deficit := 1 - b.tokens
	return false, time.Duration(deficit / l.rps * float64(time.Second))
}

// Prune removes any key whose bucket hasn't been touched in longer than
// maxAge, bounding memory growth from a stream of distinct keys (e.g.
// many client IPs) that each only ever make a handful of requests. Call
// periodically -- see Run.
func (l *Limiter) Prune(maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-maxAge)
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// Run prunes buckets untouched for longer than maxAge, every interval,
// until ctx is canceled -- the same "interval poll, not a background
// goroutine per key" posture kairon-controller/kairon-node's own reconcile
// loops already use. Start it in its own goroutine alongside the server
// using this Limiter.
func (l *Limiter) Run(ctx context.Context, interval, maxAge time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Prune(maxAge)
		}
	}
}
