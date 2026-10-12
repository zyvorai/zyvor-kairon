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

// Rebalance defaults, used when the matching RebalanceOptions field is 0.
const (
	DefaultRebalanceThresholdPercent      = 20
	DefaultRebalanceMinImprovementPercent = 5
	DefaultRebalanceMaxMoves              = 1
)

// RebalanceOptions tunes PlanRebalance.
type RebalanceOptions struct {
	// ThresholdPercent is the imbalance, in percentage points between the
	// most and least utilized node, at which moves are planned.
	ThresholdPercent int
	// MinImprovementPercent is how much a single move must lower the
	// imbalance to be worth a migration.
	MinImprovementPercent int
	// MaxMoves caps the moves in one plan.
	MaxMoves int
	// LiveOnly skips Machines live migration cannot move (non-qemu
	// backends).
	LiveOnly bool
	// Busy holds namespace/name of Machines with a migration in flight.
	Busy map[string]bool
	// Cooldown keeps a Machine moved less than this long ago in place,
	// judged from model.AnnotationDRSMovedAt against Now.
	Cooldown time.Duration
	Now      time.Time
}

func (o RebalanceOptions) threshold() int {
	if o.ThresholdPercent > 0 {
		return o.ThresholdPercent
	}
	return DefaultRebalanceThresholdPercent
}

func (o RebalanceOptions) minImprovement() int {
	if o.MinImprovementPercent > 0 {
		return o.MinImprovementPercent
	}
	return DefaultRebalanceMinImprovementPercent
}

func (o RebalanceOptions) maxMoves() int {
	if o.MaxMoves > 0 {
		return o.MaxMoves
	}
	return DefaultRebalanceMaxMoves
}

// NodeUtilization is one node's reserved share of its capacity. Percent is
// the larger of CPU and memory, which is what the planner balances.
type NodeUtilization struct {
	Node          string `json:"node"`
	CPUPercent    int    `json:"cpuPercent"`
	MemoryPercent int    `json:"memoryPercent"`
	Percent       int    `json:"percent"`
	Machines      int    `json:"machines"`
}

// RebalanceMove is one planned migration.
type RebalanceMove struct {
	Namespace string `json:"namespace"`
	Machine   string `json:"machine"`
	From      string `json:"from"`
	To        string `json:"to"`
	// Imbalance is the cluster imbalance after this move (and every move
	// before it in the plan).
	Imbalance int `json:"imbalance"`
}

// RebalancePlan is PlanRebalance's result. Reason explains an empty Moves.
type RebalancePlan struct {
	Nodes           []NodeUtilization `json:"nodes"`
	ImbalanceBefore int               `json:"imbalanceBefore"`
	ImbalanceAfter  int               `json:"imbalanceAfter"`
	Moves           []RebalanceMove   `json:"moves"`
	Reason          string            `json:"reason,omitempty"`
}

// PlanRebalance proposes up to MaxMoves migrations that lower the spread
// between the most and least utilized nodes, measured on reserved CPU and
// memory (the same accounting Choose uses), not on measured guest demand.
// Only Ready, schedulable nodes with allocatable capacity and no memory
// pressure take part, as sources or targets. Every move must pass the same
// hard filters Choose applies to the target, including required
// (anti-)affinity and DoNotSchedule spread constraints. Machines with
// passthrough devices, pinned CPUs or a topology request are never moved,
// nor are Machines annotated model.AnnotationDRS=disabled.
func (s Scheduler) PlanRebalance(nodes []model.Node, machines []model.Machine, opts RebalanceOptions) RebalancePlan {
	var pool []model.Node
	for _, n := range nodes {
		if ok, _ := s.rebalanceNode(n); ok {
			pool = append(pool, n)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Metadata.Name < pool[j].Metadata.Name })
	sim := append([]model.Machine(nil), machines...)
	load := BuildNodeLoad(sim)
	plan := RebalancePlan{Nodes: utilizations(pool, load)}
	plan.ImbalanceBefore = imbalance(plan.Nodes)
	plan.ImbalanceAfter = plan.ImbalanceBefore
	if len(pool) < 2 {
		plan.Reason = fmt.Sprintf("%d eligible node(s); rebalancing needs at least 2", len(pool))
		return plan
	}
	inPool := map[string]bool{}
	for _, n := range pool {
		inPool[n.Metadata.Name] = true
	}
	busy := make(map[string]bool, len(opts.Busy))
	for k, v := range opts.Busy {
		busy[k] = v
	}
	opts.Busy = busy

	for len(plan.Moves) < opts.maxMoves() {
		current := imbalance(utilizations(pool, load))
		if current < opts.threshold() {
			if len(plan.Moves) == 0 {
				plan.Reason = fmt.Sprintf("imbalance %d%% is below the %d%% threshold", current, opts.threshold())
			}
			break
		}
		best, bestImbalance, found := -1, current, false
		var bestTarget string
		for i, m := range sim {
			if !inPool[m.Spec.NodeName] || !rebalanceCandidate(m, opts) {
				continue
			}
			for _, target := range pool {
				if target.Metadata.Name == m.Spec.NodeName {
					continue
				}
				if ok, _ := s.eligible(m, target, nodes, sim, load[target.Metadata.Name]); !ok {
					continue
				}
				if len(s.filterMaxSkew(m, []model.Node{target}, nodes, sim)) == 0 {
					continue
				}
				after := moveLoad(load, m, m.Spec.NodeName, target.Metadata.Name)
				util := utilizations(pool, after)
				if percentOf(util, target.Metadata.Name) >= percentOf(utilizations(pool, load), m.Spec.NodeName) {
					continue // the target would end up hotter than the source was: no gain, and the next round would move it back
				}
				if v := imbalance(util); v < bestImbalance {
					best, bestImbalance, bestTarget, found = i, v, target.Metadata.Name, true
				}
			}
		}
		if !found || current-bestImbalance < opts.minImprovement() {
			if len(plan.Moves) == 0 {
				plan.Reason = fmt.Sprintf("imbalance %d%%: no single eligible move lowers it by %d points or more", current, opts.minImprovement())
			}
			break
		}
		m := sim[best]
		load = moveLoad(load, m, m.Spec.NodeName, bestTarget)
		plan.Moves = append(plan.Moves, RebalanceMove{
			Namespace: m.Namespace(), Machine: m.Metadata.Name,
			From: m.Spec.NodeName, To: bestTarget, Imbalance: bestImbalance,
		})
		sim[best].Spec.NodeName = bestTarget
		busy[m.Namespace()+"/"+m.Metadata.Name] = true
	}
	plan.ImbalanceAfter = imbalance(utilizations(pool, load))
	return plan
}

// rebalanceNode reports whether n can take part in rebalancing.
func (s Scheduler) rebalanceNode(n model.Node) (bool, string) {
	if n.Spec.Unschedulable || !ready(n) {
		return false, "unschedulable or not Ready"
	}
	if s.RequireCapableLabel && n.Metadata.Labels[model.CapableLabel] != "true" {
		return false, "not Kairon-capable"
	}
	if MemoryPressure(n) {
		return false, model.NodeConditionKaironMemoryPressure
	}
	if model.MaintenanceRequested(n) {
		return false, "in maintenance"
	}
	if cpu, mem, ok := nodeAllocatable(n); !ok || cpu <= 0 || mem == 0 {
		return false, "no allocatable capacity"
	}
	return true, ""
}

func rebalanceCandidate(m model.Machine, opts RebalanceOptions) bool {
	if m.Metadata.DeletionTimestamp != nil || m.DesiredPowerState() != "Running" || m.Status.Phase != "Running" {
		return false
	}
	if strings.EqualFold(m.Metadata.Annotations[model.AnnotationDRS], "disabled") {
		return false
	}
	if opts.Busy[m.Namespace()+"/"+m.Metadata.Name] {
		return false
	}
	if len(m.Spec.Devices) > 0 || len(m.Spec.DeviceClaims) > 0 || m.Metadata.Annotations[model.AnnotationVFIOBDF] != "" {
		return false
	}
	if m.Spec.Resources.CPUPinning || m.Spec.NeedsTopology() {
		return false
	}
	if opts.LiveOnly {
		switch m.Spec.Runtime.Backend {
		case "", "auto", "qemu":
		default:
			return false
		}
	}
	if opts.Cooldown > 0 {
		if at, err := time.Parse(time.RFC3339, m.Metadata.Annotations[model.AnnotationDRSMovedAt]); err == nil && opts.Now.Sub(at) < opts.Cooldown {
			return false
		}
	}
	return true
}

// moveLoad returns a copy of load with m's reservation moved from one node
// to another.
func moveLoad(load map[string]NodeLoad, m model.Machine, from, to string) map[string]NodeLoad {
	out := make(map[string]NodeLoad, len(load))
	for k, v := range load {
		out[k] = v
	}
	cpu, mem := machineRequests(m)
	g := guaranteedMemoryMiB(m, mem)
	f := out[from]
	f.Count--
	f.CPUMilli -= cpu
	f.MemoryMiB -= min(mem, f.MemoryMiB)
	f.GuaranteedMemoryMiB -= min(g, f.GuaranteedMemoryMiB)
	out[from] = f
	Reserve(out, to, m)
	return out
}

func utilizations(pool []model.Node, load map[string]NodeLoad) []NodeUtilization {
	out := make([]NodeUtilization, 0, len(pool))
	for _, n := range pool {
		cpu, mem, _ := nodeAllocatable(n)
		l := load[n.Metadata.Name]
		u := NodeUtilization{Node: n.Metadata.Name, Machines: l.Count}
		if cpu > 0 {
			u.CPUPercent = int(l.CPUMilli * 100 / cpu)
		}
		if mem > 0 {
			limit := uint64(float64(mem) * MemoryOvercommitRatio(n))
			u.MemoryPercent = 100 - remainingPercent(l.MemoryMiB, limit)
			if g := 100 - remainingPercent(l.GuaranteedMemoryMiB, mem); g > u.MemoryPercent {
				u.MemoryPercent = g
			}
		}
		u.Percent = max(u.CPUPercent, u.MemoryPercent)
		out = append(out, u)
	}
	return out
}

func imbalance(util []NodeUtilization) int {
	if len(util) < 2 {
		return 0
	}
	lo, hi := util[0].Percent, util[0].Percent
	for _, u := range util[1:] {
		lo = min(lo, u.Percent)
		hi = max(hi, u.Percent)
	}
	return hi - lo
}

func percentOf(util []NodeUtilization, node string) int {
	for _, u := range util {
		if u.Node == node {
			return u.Percent
		}
	}
	return 0
}
