// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package ebpfedge

import (
	"strings"
	"sync"
)

// DropCounters retains the last bounded drop snapshot for each live Machine.
// Updating one Machine only visits its own series, rather than scanning every
// Machine's flow keys. The caller caps each snapshot and calls Prune after a
// successful authoritative Machine listing.
type DropCounters struct {
	mu        sync.Mutex
	byMachine map[string]map[string]uint64
}

// Observe computes increments and replaces a Machine's previous snapshot.
// A smaller counter means the dataplane reset or evicted/recreated that flow.
// Input maps are copied; later caller mutations cannot change the baseline.
func (c *DropCounters) Observe(machinePrefix string, totals map[string]uint64) map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byMachine == nil {
		c.byMachine = make(map[string]map[string]uint64)
	}
	previous := c.byMachine[machinePrefix]
	if previous == nil {
		previous = make(map[string]uint64, len(totals))
	}
	for key := range previous {
		if _, exists := totals[key]; !exists {
			delete(previous, key)
		}
	}
	deltas := make(map[string]uint64)
	for key, now := range totals {
		if !strings.HasPrefix(key, machinePrefix) {
			continue
		}
		last := previous[key]
		if now > last {
			deltas[key] = now - last
		} else if now < last {
			deltas[key] = now
		}
		previous[key] = now
	}
	if len(previous) == 0 {
		delete(c.byMachine, machinePrefix)
	} else {
		c.byMachine[machinePrefix] = previous
	}
	return deltas
}

// Prune removes Machines no longer assigned locally. Never call it after a
// failed list, which is not evidence that the Machines were deleted.
func (c *DropCounters) Prune(activePrefixes map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for prefix := range c.byMachine {
		if !activePrefixes[prefix] {
			delete(c.byMachine, prefix)
		}
	}
}
