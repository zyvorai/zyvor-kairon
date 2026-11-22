// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"net/http"

	"github.com/zyvorai/kairon/internal/model"
)

// handleNodeUsage backs GET /api/v1/nodes/usage: the same kubectl-top-
// node-style per-node CPU/memory rollup `kaironctl top nodes` has printed
// since it shipped, now surfaced on kairon-ui's Nodes dashboard page too
// (web/src/pages/Nodes.tsx) so an operator watching the dashboard alone
// -- never touching kaironctl or kubectl -- can spot a hot node the same
// way. Cluster-wide by construction, exactly like `kaironctl top nodes`
// itself and this package's own handleListNodes/handleOverview: a
// Machine's Spec.NodeName can span namespaces onto the same physical
// node, so this deliberately calls ListMachines (every namespace), not
// ListMachinesNamespace(namespaceParam(r)).
//
// No new metrics pipeline and no new per-node aggregation logic of its
// own: model.AggregateUsageByNode (internal/model/usage.go) is the exact
// same grouping/summing function `kaironctl top nodes` calls, so the CLI
// and the dashboard can never drift apart on what counts as "this
// node's usage".
func (s *Server) handleNodeUsage(w http.ResponseWriter, r *http.Request) {
	machines, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.AggregateUsageByNode(machines))
}
