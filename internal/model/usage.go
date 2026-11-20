// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import "sort"

// NodeUsageAggregate is one row of a per-node resource-usage rollup --
// every Machine's own Status.ResourceUsage summed by the node it's
// scheduled onto (Spec.NodeName). Exported, with JSON tags, because it
// backs two independent surfaces that must never drift apart from one
// another: kaironctl's `top nodes` (internal/kaironctl's cmdTop) and
// kairon-ui's Nodes dashboard page (internal/uiapi's GET
// /api/v1/nodes/usage) both call AggregateUsageByNode below rather than
// each keeping its own copy of this grouping/summing logic.
type NodeUsageAggregate struct {
	Node        string  `json:"node"`
	Machines    int     `json:"machines"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryBytes uint64  `json:"memoryBytes"`
}

// AggregateUsageByNode groups machines by Spec.NodeName ("-" for a
// not-yet-scheduled Machine, matching `kaironctl get machines`' own
// dash(m.Spec.NodeName) rendering, so an unscheduled Machine's usage --
// if it somehow has any -- is never silently dropped nor attributed to a
// real node) and returns one NodeUsageAggregate per distinct node,
// sorted by node name for deterministic, diffable output (map iteration
// order is otherwise unspecified).
//
// A Machine with no ResourceUsage yet (never reported by its agent, or
// not yet scheduled) still counts toward Machines -- a caller asking
// "how many Machines are on this node" wants that answer regardless of
// whether usage stats have arrived -- but contributes zero to the
// CPU/memory sums: "not yet reported" and "using nothing" are
// indistinguishable from a summed total's point of view, and
// undercounting is the safer direction for a hotspot-spotting view than
// fabricating a number.
func AggregateUsageByNode(machines []Machine) []NodeUsageAggregate {
	byNode := make(map[string]*NodeUsageAggregate)
	var order []string
	for _, m := range machines {
		node := m.Spec.NodeName
		if node == "" {
			node = "-"
		}
		agg, ok := byNode[node]
		if !ok {
			agg = &NodeUsageAggregate{Node: node}
			byNode[node] = agg
			order = append(order, node)
		}
		agg.Machines++
		if u := m.Status.ResourceUsage; u != nil {
			agg.CPUPercent += u.CPUPercent
			agg.MemoryBytes += u.MemoryBytes
		}
	}
	sort.Strings(order)
	out := make([]NodeUsageAggregate, 0, len(order))
	for _, node := range order {
		out = append(out, *byNode[node])
	}
	return out
}
