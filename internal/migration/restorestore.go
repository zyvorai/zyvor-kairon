// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package migration

import (
	"sync"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

// RestoreStore keeps the last conntrack restore for a Machine so the
// destination agent can project it onto status.network.edge. The migration
// server and the agent share one store; a nil store skips recording.
type RestoreStore struct {
	mu   sync.Mutex
	last map[string]ebpfedge.RestoreResult
	// Observe, when set, is called with every recorded restore (metrics).
	Observe func(restored int, blackholeMs int64)
}

// NewRestoreStore returns an empty store.
func NewRestoreStore() *RestoreStore {
	return &RestoreStore{last: map[string]ebpfedge.RestoreResult{}}
}

func restoreKey(namespace, machine string) string {
	return namespace + "/" + machine
}

// Put records a successful restore.
func (s *RestoreStore) Put(namespace, machine string, res ebpfedge.RestoreResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.last == nil {
		s.last = map[string]ebpfedge.RestoreResult{}
	}
	s.last[restoreKey(namespace, machine)] = res
	observe := s.Observe
	s.mu.Unlock()
	if observe != nil {
		observe(res.Restored, res.BlackholeWindowMs)
	}
}

// Get returns the last restore for a Machine.
func (s *RestoreStore) Get(namespace, machine string) (ebpfedge.RestoreResult, bool) {
	if s == nil {
		return ebpfedge.RestoreResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, ok := s.last[restoreKey(namespace, machine)]
	return res, ok
}
