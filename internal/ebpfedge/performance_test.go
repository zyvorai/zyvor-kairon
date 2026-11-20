// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package ebpfedge

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDropCountersResetEvictionAndPrune(t *testing.T) {
	var c DropCounters
	totals := map[string]uint64{"a/x": 10}
	if got := c.Observe("a/", totals); got["a/x"] != 10 {
		t.Fatal(got)
	}
	totals["a/x"] = 14
	if got := c.Observe("a/", totals); got["a/x"] != 4 {
		t.Fatal("caller mutated the cached baseline", got)
	}
	if got := c.Observe("a/", map[string]uint64{"a/x": 2}); got["a/x"] != 2 {
		t.Fatal("counter reset", got)
	}
	c.Observe("b/", map[string]uint64{"b/x": 7})
	c.Observe("a/", nil)
	if len(c.byMachine) != 1 {
		t.Fatal("empty snapshot retained")
	}
	if got := c.Observe("b/", map[string]uint64{"b/x": 7}); len(got) != 0 {
		t.Fatal("another Machine changed", got)
	}
	c.Prune(map[string]bool{"a/": true})
	if len(c.byMachine) != 0 {
		t.Fatal("deleted Machine retained")
	}
	if got := c.Observe("a/", map[string]uint64{"b/x": 100}); len(got) != 0 || len(c.byMachine) != 0 {
		t.Fatal("accepted another Machine's series")
	}
}

func TestDropCountersConcurrentMachines(t *testing.T) {
	var c DropCounters
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prefix := fmt.Sprintf("ns/vm-%d/", i)
			for n := uint64(1); n <= 100; n++ {
				if got := c.Observe(prefix, map[string]uint64{prefix + "flow": n}); got[prefix+"flow"] != 1 {
					t.Errorf("lost increment: %v", got)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestConntrackTransferBoundsAndValidation(t *testing.T) {
	valid := ConntrackEntry{Proto: "tcp", SrcIP: "10.0.0.1", DstIP: "10.0.0.2", SrcPort: 1234, DstPort: 443, State: "established"}
	entries := make([]ConntrackEntry, ConntrackMaxEntries, ConntrackMaxEntries+1)
	for i := range entries {
		entries[i] = valid
	}
	if err := ValidateConntrackEntries(entries); err != nil {
		t.Fatal(err)
	}
	if err := ValidateConntrackEntries(append(entries, valid)); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	for _, proto := range []string{"tcp", "udp", "sctp"} {
		e := valid
		e.Proto = proto
		if err := ValidateConntrackEntries([]ConntrackEntry{e}); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*ConntrackEntry){
		func(e *ConntrackEntry) { e.Proto = "icmp" },
		func(e *ConntrackEntry) { e.DstIP = "::1" },
		func(e *ConntrackEntry) { e.SrcIP = "fe80::1%eth0"; e.DstIP = "fe80::2" },
		func(e *ConntrackEntry) { e.State = strings.Repeat("x", 65) },
	} {
		bad := valid
		mutate(&bad)
		snap := ConntrackSnapshot{Identity: 1, ExportedAt: time.Now(), Entries: []ConntrackEntry{valid, bad}}
		if _, err := RestoreConntrack(1, snap, time.Now()); err == nil {
			t.Fatal("bad late entry accepted")
		}
		if _, err := MarshalSnapshot(snap); err == nil {
			t.Fatal("bad snapshot serialized")
		}
	}
	if _, err := UnmarshalSnapshot([]byte(strings.Repeat(" ", ConntrackMaxSnapshotBytes+1))); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("byte bound: %v", err)
	}
	if _, err := UnmarshalSnapshot([]byte(`{"identity":1,"exportedAt":"2026-10-07T00:00:00Z","entries":[{"proto":"tcp","srcIP":"bad","dstIP":"10.0.0.2"}]}`)); err == nil {
		t.Fatal("malformed entry decoded")
	}
}

// Mirrors the parent revision's whole-node scan for an apples-to-apples
// bookkeeping benchmark. It is test-only and is not used by the agent.
func legacyDropDeltas(all map[string]uint64, prefix string, totals map[string]uint64) map[string]uint64 {
	for key := range all {
		if _, ok := totals[key]; !ok && strings.HasPrefix(key, prefix) {
			delete(all, key)
		}
	}
	out := map[string]uint64{}
	for key, now := range totals {
		last := all[key]
		if now > last {
			out[key] = now - last
		} else if now < last {
			out[key] = now
		}
		all[key] = now
	}
	return out
}

func BenchmarkDropCounters(b *testing.B) {
	for _, machines := range []int{1, 100, 1000} {
		for _, legacy := range []bool{true, false} {
			b.Run(fmt.Sprintf("machines=%d/legacy=%v", machines, legacy), func(b *testing.B) {
				var c DropCounters
				var legacyMu sync.Mutex
				all := make(map[string]uint64)
				var target map[string]uint64
				for i := 0; i < machines; i++ {
					prefix := fmt.Sprintf("ns/vm-%04d/", i)
					totals := make(map[string]uint64)
					for j := 0; j < 64; j++ {
						key := fmt.Sprintf("%sflow-%d", prefix, j)
						totals[key] = 1
						all[key] = 1
					}
					c.Observe(prefix, totals)
					if i == 0 {
						target = totals
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if legacy {
						legacyMu.Lock()
						legacyDropDeltas(all, "ns/vm-0000/", target)
						legacyMu.Unlock()
					} else {
						c.Observe("ns/vm-0000/", target)
					}
				}
			})
		}
	}
}
