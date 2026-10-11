// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestNodeTopologyRoute(t *testing.T) {
	fk := newFakeKube()
	nt := model.NodeTopology{Metadata: model.ObjectMeta{Name: "w1"}}
	nt.Status.NUMABinding = true
	nt.Status.NUMANodes = []model.TopologyNUMANode{{ID: 0, CPUs: "0-7", MemoryMiB: 16384}}
	fk.nodeTopologies = []model.NodeTopology{nt}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "vm", Namespace: "default"}}
	m.Spec.NodeName = "w1"
	m.Spec.Topology = &model.TopologySpec{Policy: model.TopologyPolicySingleNUMANode}
	m.Spec.TopologyAllocation = &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{{ID: 0, CPUs: "0-1", MemoryMiB: 1024}}}
	fk.machines["vm"] = m
	other := model.Machine{Metadata: model.ObjectMeta{Name: "elsewhere", Namespace: "default"}}
	other.Spec.NodeName = "w2"
	fk.machines["elsewhere"] = other
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/w1/topology", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got nodeTopologyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Topology == nil || !got.Topology.NUMABinding || len(got.Allocations) != 1 || got.Allocations[0].Policy != "single-numa-node" {
		t.Fatalf("%+v", got)
	}

	rr = doJSON(t, h, http.MethodGet, "/api/v1/nodes/w2/topology", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("a node without NodeTopology is not an error: %d %s", rr.Code, rr.Body.String())
	}
	got = nodeTopologyResponse{}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Topology != nil || len(got.Allocations) != 0 {
		t.Fatalf("%+v", got)
	}
}
