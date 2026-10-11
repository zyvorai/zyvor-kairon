// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/topology"
)

// TopologyUnsatisfiable prefixes every reason a node fails a Machine's
// topology request.
const TopologyUnsatisfiable = "TopologyUnsatisfiable"

// AllocateTopology places m on node n with the topology allocator. ok is
// false when n has no NodeTopology; reason is set when nothing fits.
func AllocateTopology(m model.Machine, n model.Node, machines []model.Machine, topo TopologyInput) (alloc topology.Allocation, score int, reason string, ok bool) {
	nt, has := topo.Nodes[n.Metadata.Name]
	if !has {
		return topology.Allocation{}, 0, "", false
	}
	r, err := topology.RequestFor(m)
	if err != nil {
		return topology.Allocation{}, 0, err.Error(), true
	}
	if m.Spec.HardTopology() && len(topo.DRA.BDFs) > 0 {
		r.RequiredNUMA = topology.NodesForBDFs(nt, topo.DRA.BDFs)
	}
	alloc, score, reason = topology.Allocate(topology.BuildGraph(nt, n, machines, m), r)
	return alloc, score, reason, true
}

// topologyEligible is the topology half of eligible.
func topologyEligible(m model.Machine, n model.Node, machines []model.Machine, topo TopologyInput) (bool, string) {
	if !m.Spec.NeedsTopology() {
		return true, ""
	}
	if m.Spec.HardTopology() && topo.DRA.Node != "" && topo.DRA.Node != n.Metadata.Name {
		return false, fmt.Sprintf("a deviceClaim is allocated on node %s", topo.DRA.Node)
	}
	_, _, reason, ok := AllocateTopology(m, n, machines, topo)
	if !ok {
		// best-effort with no devices never refuses a node over locality.
		if m.Spec.TopologyPolicy() == model.TopologyPolicyBestEffort && len(m.Spec.Devices) == 0 {
			return true, ""
		}
		return false, TopologyUnsatisfiable + ": node has no NodeTopology"
	}
	if reason != "" {
		return false, TopologyUnsatisfiable + ": " + reason
	}
	return true, ""
}

// topologyScore adds up to 20 points for locality (one NUMA node beats
// two, local devices beat remote ones).
func topologyScore(m model.Machine, n model.Node, machines []model.Machine, topo TopologyInput) int {
	if !m.Spec.NeedsTopology() {
		return 0
	}
	_, score, reason, ok := AllocateTopology(m, n, machines, topo)
	if !ok || reason != "" {
		return 0
	}
	return score / 5
}
