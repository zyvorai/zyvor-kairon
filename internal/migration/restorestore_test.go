// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package migration

import (
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

func TestRestoreStoreRoundTrip(t *testing.T) {
	s := NewRestoreStore()
	want := ebpfedge.RestoreResult{Restored: 3, BlackholeWindowMs: 40, Identity: 9}
	s.Put("demo", "web", want)
	got, ok := s.Get("demo", "web")
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if _, ok := s.Get("demo", "other"); ok {
		t.Fatal("unexpected hit")
	}
	var nilStore *RestoreStore
	nilStore.Put("demo", "web", want)
	if _, ok := nilStore.Get("demo", "web"); ok {
		t.Fatal("nil store should miss")
	}
	_ = time.Now()
}

func TestRestoreStoreObserve(t *testing.T) {
	s := NewRestoreStore()
	var restored int
	var window int64
	s.Observe = func(n int, ms int64) { restored, window = n, ms }
	s.Put("demo", "web", ebpfedge.RestoreResult{Restored: 7, BlackholeWindowMs: 12})
	if restored != 7 || window != 12 {
		t.Fatalf("observed %d/%d", restored, window)
	}
}
