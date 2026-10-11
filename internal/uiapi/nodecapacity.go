// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"sort"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// nodeCapacity is one row of GET /api/v1/nodes/capacity.
type nodeCapacity struct {
	Node            string `json:"node"`
	Ready           bool   `json:"ready"`
	Unschedulable   bool   `json:"unschedulable"`
	Taints          int    `json:"taints"`
	Machines        int    `json:"machines"`
	AllocCPUMilli   int64  `json:"allocatableCpuMilli"`
	AllocMemoryMiB  uint64 `json:"allocatableMemoryMiB"`
	ReqCPUMilli     int64  `json:"requestedCpuMilli"`
	ReqMemoryMiB    uint64 `json:"requestedMemoryMiB"`
	KnownAllocation bool   `json:"knownAllocatable"`
	// GuaranteedMemoryMiB counts elastic-memory Machines at their minimum.
	GuaranteedMemoryMiB uint64 `json:"guaranteedMemoryMiB"`
	// MemoryOvercommitRatio is the node's
	// kairon.zyvor.dev/memory-overcommit-ratio (1 when unset).
	MemoryOvercommitRatio float64 `json:"memoryOvercommitRatio"`
	// MemoryPressure is the node's KaironMemoryPressure condition.
	MemoryPressure bool `json:"memoryPressure"`
	// WhatIf is present only when ?cpu= and ?memory= were given.
	WhatIf *whatIfResult `json:"whatIf,omitempty"`
}

type whatIfResult struct {
	Fits   bool   `json:"fits"`
	Reason string `json:"reason,omitempty"`
}

// handleNodeCapacity serves GET /api/v1/nodes/capacity: per node,
// allocatable versus what scheduled Machines request (the scheduler's own
// accounting, scheduler.BuildNodeLoad), plus an optional what-if
// "would a Machine of ?cpu=4&memory=8Gi fit here?" using scheduler.Fits.
func (s *Server) handleNodeCapacity(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Kube.ListNodes(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	machines, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	var probe *model.Machine
	if cpu, mem := r.URL.Query().Get("cpu"), r.URL.Query().Get("memory"); cpu != "" || mem != "" {
		if _, err := model.ParseVCPUs(cpu); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cpu: "+err.Error())
			return
		}
		if _, err := model.ParseMemoryMiB(mem); err != nil {
			writeError(w, http.StatusBadRequest, "invalid memory: "+err.Error())
			return
		}
		var m model.Machine
		m.Spec.Resources.CPU = cpu
		m.Spec.Resources.Memory = mem
		probe = &m
	}
	load := scheduler.BuildNodeLoad(machines)
	out := make([]nodeCapacity, 0, len(nodes))
	for _, n := range nodes {
		l := load[n.Metadata.Name]
		row := nodeCapacity{
			Node: n.Metadata.Name, Unschedulable: n.Spec.Unschedulable, Taints: len(n.Spec.Taints),
			Machines: l.Count, ReqCPUMilli: l.CPUMilli, ReqMemoryMiB: l.MemoryMiB,
			GuaranteedMemoryMiB:   l.GuaranteedMemoryMiB,
			MemoryOvercommitRatio: scheduler.MemoryOvercommitRatio(n),
			MemoryPressure:        scheduler.MemoryPressure(n),
		}
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				row.Ready = true
			}
		}
		row.AllocCPUMilli, row.AllocMemoryMiB, row.KnownAllocation = scheduler.Allocatable(n)
		if probe != nil {
			fits, reason := scheduler.Fits(n, *probe, l)
			if fits && (!row.Ready || row.Unschedulable) {
				fits, reason = false, "node is not Ready or is unschedulable"
			}
			if fits && row.MemoryPressure {
				fits, reason = false, "node reports "+model.NodeConditionKaironMemoryPressure
			}
			row.WhatIf = &whatIfResult{Fits: fits, Reason: reason}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	writeJSON(w, http.StatusOK, out)
}
