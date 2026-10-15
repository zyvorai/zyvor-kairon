// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/model"
)

type FlowCollectionStats struct {
	External  int `json:"externalFlowsSkipped"`
	Denied    int `json:"deniedFlowsSkipped"`
	Reset     int `json:"counterResetsSkipped"`
	Unbounded int `json:"unboundedNewFlowsSkipped"`
}

// DecodeMeasuredFlows accepts the canonical FluxVM VM-edge flow envelope. It
// refuses unknown counter schemas instead of treating absent byte counts as 0.
func DecodeMeasuredFlows(raw json.RawMessage) ([]ebpfedge.FlowRecord, error) {
	var doc struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Items) == 0 {
		return nil, fmt.Errorf("flow response needs canonical items array")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(doc.Items, &rows); err != nil {
		return nil, err
	}
	if len(rows) > 10000 {
		return nil, fmt.Errorf("flow response exceeds 10000 records")
	}
	out := make([]ebpfedge.FlowRecord, 0, len(rows))
	for _, row := range rows {
		var required map[string]json.RawMessage
		if err := json.Unmarshal(row, &required); err != nil {
			return nil, err
		}
		if required["bytes"] == nil || required["srcIP"] == nil || required["dstIP"] == nil {
			return nil, fmt.Errorf("flow schema needs bytes, srcIP and dstIP")
		}
		var flow ebpfedge.FlowRecord
		if err := json.Unmarshal(row, &flow); err != nil {
			return nil, err
		}
		out = append(out, flow)
	}
	return out, nil
}

// TrafficFromCounters derives rates from two cumulative egress snapshots.
// It never divides lifetime counters by a guessed sampling window. External,
// denied, reset and pre-existing-but-newly-visible flows are counted separately.
func TrafficFromCounters(machines []model.Machine, before, after map[string][]ebpfedge.FlowRecord, start, end time.Time) ([]TrafficEdge, FlowCollectionStats, error) {
	stats := FlowCollectionStats{}
	duration := end.Sub(start).Seconds()
	if duration <= 0 || duration > 300 {
		return nil, stats, fmt.Errorf("counter sampling window must be 0..300 seconds")
	}
	identities := map[string]string{}
	ip := func(s string) string {
		a, e := netip.ParseAddr(s)
		if e != nil {
			return ""
		}
		return a.Unmap().String()
	}
	for _, m := range machines {
		id := m.Namespace() + "/" + m.Metadata.Name
		addresses := append([]string{m.Status.GuestIP}, m.Status.GuestIPs...)
		for _, address := range addresses {
			if address == "" {
				continue
			}
			key := ip(address)
			if key == "" {
				return nil, stats, fmt.Errorf("machine has invalid guest IP")
			}
			if other := identities[key]; other != "" && other != id {
				return nil, stats, fmt.Errorf("guest IP %s is ambiguous between Machines", key)
			}
			identities[key] = id
		}
	}
	rates := map[string]float64{}
	pairs := map[string][2]string{}
	for id, flows := range after {
		old := map[ebpfedge.FlowKey]ebpfedge.FlowRecord{}
		for _, f := range before[id] {
			if _, ok := old[f.FlowKey]; ok {
				return nil, stats, fmt.Errorf("duplicate baseline flow key")
			}
			old[f.FlowKey] = f
		}
		seen := map[ebpfedge.FlowKey]bool{}
		for _, f := range flows {
			if seen[f.FlowKey] {
				return nil, stats, fmt.Errorf("duplicate sampled flow key")
			}
			seen[f.FlowKey] = true
			if f.Denied {
				stats.Denied++
				continue
			}
			from, to := identities[ip(f.SrcIP)], identities[ip(f.DstIP)]
			if from == "" || to == "" {
				stats.External++
				continue
			}
			if from != id {
				continue
			} // destination-side observations must not double count the same traffic.
			if from == to {
				continue
			}
			prior, known := old[f.FlowKey]
			var delta uint64
			if known {
				if f.Bytes < prior.Bytes || !f.FirstSeen.Equal(prior.FirstSeen) {
					stats.Reset++
					continue
				}
				delta = f.Bytes - prior.Bytes
			} else {
				if f.FirstSeen.Before(start) || f.FirstSeen.After(end) {
					stats.Unbounded++
					continue
				}
				delta = f.Bytes
			}
			if delta == 0 {
				continue
			}
			key := from + "\x00" + to
			rates[key] += float64(delta) / duration
			pairs[key] = [2]string{from, to}
		}
	}
	keys := make([]string, 0, len(rates))
	for k := range rates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	edges := make([]TrafficEdge, 0, len(keys))
	for _, k := range keys {
		edges = append(edges, TrafficEdge{From: pairs[k][0], To: pairs[k][1], BytesPerSecond: rates[k]})
	}
	return edges, stats, nil
}
