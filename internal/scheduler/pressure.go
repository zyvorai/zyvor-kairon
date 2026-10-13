// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// Rebalance signals: what PlanRebalance balances.
const (
	// RebalanceSignalReserved balances reserved CPU and memory.
	RebalanceSignalReserved = "reserved"
	// RebalanceSignalPressure moves Machines off nodes whose measured PSI
	// (model.AnnotationNodePressure, written by kairon-node) is high.
	RebalanceSignalPressure = "pressure"
)

// Pressure defaults, used when the matching RebalanceOptions field is 0.
const (
	DefaultPressureThresholdPercent = 25
	DefaultPressureMaxAge           = 2 * time.Minute
)

func (o RebalanceOptions) pressureThreshold() float64 {
	if o.PressureThresholdPercent > 0 {
		return float64(o.PressureThresholdPercent)
	}
	return DefaultPressureThresholdPercent
}

func (o RebalanceOptions) pressureMaxAge() time.Duration {
	if o.PressureMaxAge > 0 {
		return o.PressureMaxAge
	}
	return DefaultPressureMaxAge
}

// planPressure is PlanRebalance for RebalanceSignalPressure. A node is hot
// when its worst host PSI (some avg60 of cpu, memory or io) reaches the
// threshold, and calm below half of it; the gap keeps a Machine from being
// moved straight back. From each hot node, hottest first, it moves the
// Machine whose own cgroup stalls most to the calmest node that passes the
// same hard filters and reserved-capacity checks as reserved rebalancing;
// each calm node takes at most one Machine per round. Nodes without a
// sample newer than PressureMaxAge are left out: their pressure is unknown.
// ImbalanceBefore is the spread of worst host PSI between measured nodes.
// Moves carry no prediction: Imbalance stays ImbalanceBefore and Reason
// says why the Machine moves.
func (s Scheduler) planPressure(pool, nodes []model.Node, machines []model.Machine, opts RebalanceOptions) RebalancePlan {
	sim := append([]model.Machine(nil), machines...)
	load := BuildNodeLoad(sim)
	plan := RebalancePlan{Signal: RebalanceSignalPressure, Nodes: utilizations(pool, load)}

	measured := map[string]model.NodePressure{}
	var unmeasured []string
	for i, n := range pool {
		p, ok := model.NodePressureOf(n)
		age, fresh := p.Age(opts.Now)
		if !ok || !fresh || age > opts.pressureMaxAge() || age < -opts.pressureMaxAge() {
			unmeasured = append(unmeasured, n.Metadata.Name)
			continue
		}
		host := p.Host
		plan.Nodes[i].Pressure = &host
		measured[n.Metadata.Name] = p
	}
	plan.ImbalanceBefore = pressureSpread(measured)
	plan.ImbalanceAfter = plan.ImbalanceBefore
	if len(measured) < 2 {
		plan.Reason = fmt.Sprintf("%d node(s) with a pressure sample newer than %s; rebalancing needs at least 2", len(measured), opts.pressureMaxAge())
		if len(unmeasured) > 0 {
			plan.Reason += " (no fresh sample: " + strings.Join(unmeasured, ", ") + ")"
		}
		return plan
	}

	threshold := opts.pressureThreshold()
	var hot []string
	calm := map[string]bool{}
	for name, p := range measured {
		switch v := p.Host.Max(); {
		case v >= threshold:
			hot = append(hot, name)
		case v < threshold/2:
			calm[name] = true
		}
	}
	sort.Slice(hot, func(i, j int) bool {
		a, b := measured[hot[i]].Host.Max(), measured[hot[j]].Host.Max()
		if a != b {
			return a > b
		}
		return hot[i] < hot[j]
	})
	if len(hot) == 0 {
		plan.Reason = fmt.Sprintf("no node's pressure reaches the %.0f%% threshold", threshold)
		return plan
	}
	if len(calm) == 0 {
		plan.Reason = fmt.Sprintf("no node is below %.1f%% (half the threshold) to take a Machine", threshold/2)
		return plan
	}

	busy := make(map[string]bool, len(opts.Busy))
	for k, v := range opts.Busy {
		busy[k] = v
	}
	opts.Busy = busy
	byName := map[string]model.Node{}
	for _, n := range pool {
		byName[n.Metadata.Name] = n
	}

	for _, from := range hot {
		if len(plan.Moves) >= opts.maxMoves() || len(calm) == 0 {
			break
		}
		src := measured[from]
		stall := func(m model.Machine) float64 { return src.Machines[m.Namespace()+"/"+m.Metadata.Name].Max() }
		var candidates []int
		for i, m := range sim {
			if m.Spec.NodeName == from && rebalanceCandidate(m, opts) {
				candidates = append(candidates, i)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			a, b := sim[candidates[i]], sim[candidates[j]]
			if sa, sb := stall(a), stall(b); sa != sb {
				return sa > sb
			}
			return a.Namespace()+"/"+a.Metadata.Name < b.Namespace()+"/"+b.Metadata.Name
		})
		targets := make([]string, 0, len(calm))
		for name := range calm {
			targets = append(targets, name)
		}
		util := utilizations(pool, load)
		sort.Slice(targets, func(i, j int) bool {
			a, b := measured[targets[i]].Host.Max(), measured[targets[j]].Host.Max()
			if a != b {
				return a < b
			}
			if ua, ub := percentOf(util, targets[i]), percentOf(util, targets[j]); ua != ub {
				return ua < ub
			}
			return targets[i] < targets[j]
		})
	pick:
		for _, i := range candidates {
			m := sim[i]
			for _, to := range targets {
				target := byName[to]
				if ok, _ := s.eligible(m, target, nodes, sim, load[to]); !ok {
					continue
				}
				if len(s.filterMaxSkew(m, []model.Node{target}, nodes, sim)) == 0 {
					continue
				}
				resource, value := src.Host.Worst()
				plan.Moves = append(plan.Moves, RebalanceMove{
					Namespace: m.Namespace(), Machine: m.Metadata.Name, From: from, To: to,
					Imbalance: plan.ImbalanceBefore,
					Reason: fmt.Sprintf("%s pressure on %s is %.1f%% (threshold %.0f%%); %s is at %.1f%%; this Machine stalls %.1f%%",
						resource, from, value, threshold, to, measured[to].Host.Max(), stall(m)),
				})
				load = moveLoad(load, m, from, to)
				sim[i].Spec.NodeName = to
				busy[m.Namespace()+"/"+m.Metadata.Name] = true
				delete(calm, to)
				break pick
			}
		}
	}
	if len(plan.Moves) == 0 {
		plan.Reason = fmt.Sprintf("%s over the threshold, but no eligible Machine there fits a calm node", strings.Join(hot, ", "))
	}
	return plan
}

// pressureSpread is the gap, in whole points, between the most and least
// pressured measured nodes.
func pressureSpread(measured map[string]model.NodePressure) int {
	first := true
	var lo, hi float64
	for _, p := range measured {
		v := p.Host.Max()
		if first {
			lo, hi, first = v, v, false
			continue
		}
		lo, hi = min(lo, v), max(hi, v)
	}
	return int(hi - lo + 0.5)
}
