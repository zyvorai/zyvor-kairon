// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// NodeFit is one scheduler rejection. Reasons are the blocking
// constraints the scheduler already returns; this function does not
// reschedule.
type NodeFit struct {
	Name    string   `json:"name"`
	Reasons []string `json:"reasons,omitempty"`
}

type PlacementExplanation struct {
	Machine string    `json:"machine"`
	Pending bool      `json:"pending"`
	Summary string    `json:"summary"`
	Nodes   []NodeFit `json:"nodes,omitempty"`
	Hints   []string  `json:"hints,omitempty"`
}

func ExplainPending(machine string, nodes []NodeFit) PlacementExplanation {
	ex := PlacementExplanation{Machine: machine, Nodes: nodes}
	if len(nodes) == 0 {
		ex.Pending = true
		ex.Summary = "no node reported a fit decision"
		ex.Hints = []string{"scheduler has not scored this Machine yet"}
		return ex
	}
	blocked := 0
	for _, n := range nodes {
		if len(n.Reasons) > 0 {
			blocked++
		}
	}
	if blocked == 0 {
		ex.Summary = fmt.Sprintf("%s fits %d node(s); not pending on placement", machine, len(nodes))
		return ex
	}
	ex.Pending = true
	ex.Summary = fmt.Sprintf("%s is pending: %d of %d nodes rejected it", machine, blocked, len(nodes))
	ex.Hints = []string{
		"read the reason strings; do not clear a constraint to force a fit",
		"a missing storage-domain or pinnable-cpus label is an operator fact, not a scheduler bug",
	}
	return ex
}
