// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// nodeTopologyResponse is GET /api/v1/nodes/{node}/topology.
type nodeTopologyResponse struct {
	Node string `json:"node"`
	// Topology is null until kairon-node has published one.
	Topology    *model.NodeTopologyStatus `json:"topology"`
	Allocations []topologyAllocationRow   `json:"allocations"`
}

type topologyAllocationRow struct {
	Namespace  string                    `json:"namespace"`
	Name       string                    `json:"name"`
	Policy     string                    `json:"policy"`
	Allocation *model.TopologyAllocation `json:"allocation"`
	Applied    *model.AppliedTopology    `json:"applied,omitempty"`
}

func (s *Server) handleNodeTopology(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	out := nodeTopologyResponse{Node: node, Allocations: []topologyAllocationRow{}}
	nt, err := s.Kube.GetNodeTopology(r.Context(), node)
	switch {
	case err == nil:
		out.Topology = &nt.Status
	case !kube.IsNotFound(err):
		writeUpstreamError(w, err)
		return
	}
	machines, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	for _, m := range machines {
		if m.Spec.NodeName != node || m.Spec.TopologyAllocation == nil || !s.authorizedForNamespace(r.Context(), m.Namespace()) {
			continue
		}
		out.Allocations = append(out.Allocations, topologyAllocationRow{
			Namespace: m.Namespace(), Name: m.Metadata.Name, Policy: m.Spec.TopologyPolicy(),
			Allocation: m.Spec.TopologyAllocation, Applied: m.Status.AppliedTopology,
		})
	}
	sort.Slice(out.Allocations, func(i, j int) bool {
		a, b := out.Allocations[i], out.Allocations[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	writeJSON(w, http.StatusOK, out)
}
