// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package ebpfedge

import (
	"sort"
	"sync"
	"time"
)

// FlowKey identifies one L4 flow of a Machine.
type FlowKey struct {
	Identity uint32 `json:"identity"`
	Proto    string `json:"proto"`
	SrcIP    string `json:"srcIP"`
	DstIP    string `json:"dstIP"`
	SrcPort  uint16 `json:"srcPort"`
	DstPort  uint16 `json:"dstPort"`
}

// FlowRecord aggregates bytes and packets for a flow, with the last drop
// reason if any packet of it was denied.
type FlowRecord struct {
	FlowKey
	Bytes     uint64    `json:"bytes"`
	Packets   uint64    `json:"packets"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	Denied    bool      `json:"denied,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// FlowTable is a bounded, concurrency-safe table of FlowRecords. When full,
// the least recently seen flow is evicted.
type FlowTable struct {
	mu    sync.Mutex
	max   int
	flows map[FlowKey]*FlowRecord
}

// NewFlowTable returns a table that keeps at most max flows (min 1).
func NewFlowTable(max int) *FlowTable {
	if max < 1 {
		max = 1
	}
	return &FlowTable{max: max, flows: make(map[FlowKey]*FlowRecord)}
}

// Record adds one packet's bytes to its flow. A non-allow decision marks the
// flow denied with the decision's reason.
func (t *FlowTable) Record(key FlowKey, bytes uint32, d Decision, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fr, ok := t.flows[key]
	if !ok {
		if len(t.flows) >= t.max {
			t.evictOldestLocked()
		}
		fr = &FlowRecord{FlowKey: key, FirstSeen: now}
		t.flows[key] = fr
	}
	fr.Bytes += uint64(bytes)
	fr.Packets++
	fr.LastSeen = now
	if d.Action == "drop" {
		fr.Denied = true
		fr.Reason = d.Reason
	}
}

// Snapshot returns the flows, most recently seen first.
func (t *FlowTable) Snapshot() []FlowRecord {
	t.mu.Lock()
	out := make([]FlowRecord, 0, len(t.flows))
	for _, fr := range t.flows {
		out = append(out, *fr)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func (t *FlowTable) evictOldestLocked() {
	var oldest FlowKey
	var at time.Time
	first := true
	for k, fr := range t.flows {
		if first || fr.LastSeen.Before(at) {
			oldest, at, first = k, fr.LastSeen, false
		}
	}
	delete(t.flows, oldest)
}
