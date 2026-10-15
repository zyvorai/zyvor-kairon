// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// TrafficEdge is a measured VM communication rate over the collector window.
// IDs are namespace/name, not untrusted IP-to-workload guesses.
type TrafficEdge struct {
	From           string  `json:"from"`
	To             string  `json:"to"`
	BytesPerSecond float64 `json:"bytesPerSecond"`
}
type MeasuredLink struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	RTTMillis float64   `json:"rttMillis"`
	SampledAt time.Time `json:"sampledAt"`
}
type NetworkTelemetry struct {
	APIVersion        string         `json:"apiVersion"`
	CapturedAt        time.Time      `json:"capturedAt"`
	WindowSeconds     int            `json:"windowSeconds"`
	SameNodeRTTMillis float64        `json:"sameNodeRTTMillis"`
	Edges             []TrafficEdge  `json:"edges"`
	Links             []MeasuredLink `json:"links"`
}
type NetworkPlacementOptions struct {
	MaxMoves              int             `json:"maxMoves"`
	MinImprovementPercent float64         `json:"minImprovementPercent"`
	MaxPressurePercent    float64         `json:"maxPressurePercent"`
	MigrationPenalty      float64         `json:"migrationPenalty"`
	MaxAgeSeconds         int             `json:"maxAgeSeconds"`
	CooldownSeconds       int             `json:"cooldownSeconds"`
	Busy                  map[string]bool `json:"-"`
}
type NetworkMove struct {
	Namespace                  string  `json:"namespace"`
	Machine                    string  `json:"machine"`
	UID                        string  `json:"uid"`
	From                       string  `json:"from"`
	To                         string  `json:"to"`
	EstimatedSavings           float64 `json:"estimatedSavingsByteMillisPerSecond"`
	RequiresMigrationPreflight bool    `json:"requiresMigrationPreflight"`
}
type NetworkPlacementReport struct {
	CapturedAt  time.Time      `json:"capturedAt"`
	EvaluatedAt time.Time      `json:"evaluatedAt"`
	Before      float64        `json:"estimatedCostBefore"`
	After       float64        `json:"estimatedCostAfter"`
	Moves       []NetworkMove  `json:"moves"`
	Rejected    map[string]int `json:"rejected"`
	Reason      string         `json:"reason,omitempty"`
	Model       string         `json:"model"`
}

func finitePositive(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }
func pair(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// PlanNetworkPlacement is recommendation-only. Every candidate uses Kairon's
// hard filters and capacity accounting; no NodeName is patched by this planner.
// This cost model estimates traffic-weighted RTT, not application latency.
func (s Scheduler) PlanNetworkPlacement(nodes []model.Node, machines []model.Machine, t NetworkTelemetry, o NetworkPlacementOptions, now time.Time) (NetworkPlacementReport, error) {
	out := NetworkPlacementReport{CapturedAt: t.CapturedAt, EvaluatedAt: now, Rejected: map[string]int{}, Model: "sum(bytesPerSecond * measured RTT ms); estimates, not application latency"}
	if o.MaxAgeSeconds == 0 {
		o.MaxAgeSeconds = 120
	}
	if o.MaxMoves == 0 {
		o.MaxMoves = 1
	}
	if o.MinImprovementPercent == 0 {
		o.MinImprovementPercent = 10
	}
	if o.MaxPressurePercent == 0 {
		o.MaxPressurePercent = 25
	}
	if o.CooldownSeconds == 0 {
		o.CooldownSeconds = 3600
	}
	if o.MaxMoves < 1 || o.MaxMoves > 32 || o.MaxAgeSeconds < 1 || o.MaxAgeSeconds > 3600 || !finitePositive(o.MinImprovementPercent) || o.MinImprovementPercent > 100 || !finitePositive(o.MaxPressurePercent) || o.MaxPressurePercent > 100 || o.MigrationPenalty < 0 || math.IsNaN(o.MigrationPenalty) || math.IsInf(o.MigrationPenalty, 0) || o.CooldownSeconds < 0 {
		return out, fmt.Errorf("invalid network placement options")
	}
	maxAge := time.Duration(o.MaxAgeSeconds) * time.Second
	fresh := func(at time.Time) bool {
		age := now.Sub(at)
		return !at.IsZero() && age >= -5*time.Second && age <= maxAge
	}
	if t.APIVersion != "networkplacement.kairon.zyvor.dev/v1alpha1" || !fresh(t.CapturedAt) || t.WindowSeconds < 1 || t.WindowSeconds > 3600 || !finitePositive(t.SameNodeRTTMillis) || len(t.Edges) == 0 || len(t.Edges) > 100000 {
		return out, fmt.Errorf("invalid or stale network telemetry")
	}
	byNode := map[string]model.Node{}
	for _, n := range nodes {
		if byNode[n.Metadata.Name].Metadata.Name != "" {
			return out, fmt.Errorf("duplicate node")
		}
		byNode[n.Metadata.Name] = n
	}
	locations := map[string]string{}
	sim := append([]model.Machine(nil), machines...)
	for _, m := range sim {
		id := m.Namespace() + "/" + m.Metadata.Name
		if locations[id] != "" {
			return out, fmt.Errorf("duplicate Machine")
		}
		locations[id] = m.Spec.NodeName
	}
	links := map[string]float64{}
	for _, l := range t.Links {
		key := pair(l.From, l.To)
		if !fresh(l.SampledAt) || !finitePositive(l.RTTMillis) || l.From == l.To || byNode[l.From].Metadata.Name == "" || byNode[l.To].Metadata.Name == "" || links[key] != 0 {
			return out, fmt.Errorf("invalid, duplicate or stale RTT measurement")
		}
		links[key] = l.RTTMillis
	}
	for _, e := range t.Edges {
		if e.From == e.To || locations[e.From] == "" || locations[e.To] == "" || !finitePositive(e.BytesPerSecond) || e.BytesPerSecond > 1e15 {
			return out, fmt.Errorf("traffic edge needs known scheduled peers and a finite measured rate")
		}
	}
	cost := func(loc map[string]string) (float64, bool) {
		var total float64
		for _, e := range t.Edges {
			a, b := loc[e.From], loc[e.To]
			rtt := t.SameNodeRTTMillis
			if a != b {
				var ok bool
				rtt, ok = links[pair(a, b)]
				if !ok {
					return 0, false
				}
			}
			total += e.BytesPerSecond * rtt
		}
		return total, !math.IsInf(total, 0)
	}
	baseline, complete := cost(locations)
	if !complete {
		return out, fmt.Errorf("missing measured RTT for current traffic graph")
	}
	out.Before, out.After = baseline, baseline
	pool := make([]model.Node, 0, len(nodes))
	for _, n := range nodes {
		if ok, reason := s.rebalanceNode(n); !ok {
			out.Rejected[reason]++
			continue
		}
		p, ok := model.NodePressureOf(n)
		age, valid := p.Age(now)
		if !ok || !valid || age < -5*time.Second || age > maxAge || math.IsNaN(p.Host.Max()) || math.IsInf(p.Host.Max(), 0) || p.Host.Max() < 0 || p.Host.Max() >= o.MaxPressurePercent {
			out.Rejected["unknown, stale or high host pressure"]++
			continue
		}
		pool = append(pool, n)
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Metadata.Name < pool[j].Metadata.Name })
	sort.Slice(sim, func(i, j int) bool {
		return sim[i].Namespace()+"/"+sim[i].Metadata.Name < sim[j].Namespace()+"/"+sim[j].Metadata.Name
	})
	moved := map[string]bool{}
	load := BuildNodeLoad(sim)
	for round := 0; round < o.MaxMoves; round++ {
		bestIndex := -1
		bestTarget := ""
		bestCost := out.After
		for i, m := range sim {
			id := m.Namespace() + "/" + m.Metadata.Name
			if moved[id] || m.Spec.Runtime.Backend != "qemu" || !rebalanceCandidate(m, RebalanceOptions{LiveOnly: true, Busy: o.Busy, Cooldown: time.Duration(o.CooldownSeconds) * time.Second, Now: now}) {
				continue
			}
			source := byNode[m.Spec.NodeName]
			sourceEligible := false
			for _, n := range pool {
				sourceEligible = sourceEligible || n.Metadata.Name == source.Metadata.Name
			}
			if !sourceEligible {
				continue
			}
			candidates := make([]model.Node, 0, len(pool))
			loadWithout := moveLoad(load, m, m.Spec.NodeName, "")
			for _, target := range pool {
				if source.Metadata.Labels["kairon.zyvor.dev/storage-domain"] == "" || source.Metadata.Labels["kairon.zyvor.dev/network-domain"] == "" || source.Metadata.Labels["kairon.zyvor.dev/storage-domain"] != target.Metadata.Labels["kairon.zyvor.dev/storage-domain"] || source.Metadata.Labels["kairon.zyvor.dev/network-domain"] != target.Metadata.Labels["kairon.zyvor.dev/network-domain"] {
					out.Rejected["unconfirmed storage/network domains"]++
					continue
				}
				if ok, reason := s.eligible(m, target, nodes, sim, loadWithout[target.Metadata.Name]); !ok {
					out.Rejected[reason]++
					continue
				}
				candidates = append(candidates, target)
			}
			for _, target := range s.filterMaxSkew(m, candidates, nodes, sim) {
				if target.Metadata.Name == m.Spec.NodeName {
					continue
				}
				old := locations[id]
				locations[id] = target.Metadata.Name
				after, ok := cost(locations)
				locations[id] = old
				if !ok {
					out.Rejected["missing candidate RTT measurement"]++
					continue
				}
				gain := out.After - after
				if gain <= o.MigrationPenalty || gain*100/out.After < o.MinImprovementPercent {
					continue
				}
				if after < bestCost {
					bestCost, bestIndex, bestTarget = after, i, target.Metadata.Name
				}
			}
		}
		if bestIndex < 0 {
			break
		}
		m := sim[bestIndex]
		id := m.Namespace() + "/" + m.Metadata.Name
		out.Moves = append(out.Moves, NetworkMove{Namespace: m.Namespace(), Machine: m.Metadata.Name, UID: m.Metadata.UID, From: m.Spec.NodeName, To: bestTarget, EstimatedSavings: out.After - bestCost, RequiresMigrationPreflight: true})
		load = moveLoad(load, m, m.Spec.NodeName, bestTarget)
		sim[bestIndex].Spec.NodeName = bestTarget
		locations[id] = bestTarget
		moved[id] = true
		out.After = bestCost
	}
	if len(out.Moves) == 0 {
		out.Reason = "no sufficiently beneficial move passes measured-pressure, capacity and placement constraints"
	}
	return out, nil
}
