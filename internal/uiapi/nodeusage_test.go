// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

// TestNodeUsageAggregatesAcrossMachines is GET /api/v1/nodes/usage's
// happy path -- two Machines on worker-1 (usage must sum) and one on
// worker-2 (must stay separate), rows sorted by node name, mirroring
// internal/kaironctl's own TestCmdTopNodesAggregatesAcrossMachines
// exactly since both now call the same model.AggregateUsageByNode.
func TestNodeUsageAggregatesAcrossMachines(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm-1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{ResourceUsage: &model.ResourceUsage{CPUPercent: 100, MemoryBytes: 1 * 1024 * 1024 * 1024}},
	}
	fk.machines["vm-2"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-2", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{ResourceUsage: &model.ResourceUsage{CPUPercent: 50, MemoryBytes: 512 * 1024 * 1024}},
	}
	fk.machines["vm-3"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-3", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "worker-2"},
		Status:   model.MachineStatus{ResourceUsage: &model.ResourceUsage{CPUPercent: 25, MemoryBytes: 256 * 1024 * 1024}},
	}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/usage", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got []model.NodeUsageAggregate
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []model.NodeUsageAggregate{
		{Node: "worker-1", Machines: 2, CPUPercent: 150, MemoryBytes: 1536 * 1024 * 1024},
		{Node: "worker-2", Machines: 1, CPUPercent: 25, MemoryBytes: 256 * 1024 * 1024},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestNodeUsageUnscheduledMachineGroupsUnderDash covers a Machine with no
// Spec.NodeName -- it must still show up (grouped under "-"), not be
// silently dropped from the response.
func TestNodeUsageUnscheduledMachineGroupsUnderDash(t *testing.T) {
	fk := newFakeKube()
	fk.machines["vm-1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "default"},
	}
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/usage", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got []model.NodeUsageAggregate
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Node != "-" || got[0].Machines != 1 {
		t.Fatalf("expected a single unscheduled row, got %+v", got)
	}
}

// TestNodeUsageEmptyClusterReturnsEmptyArray guards against
// model.AggregateUsageByNode ever regressing into returning nil for zero
// machines: writeJSON must then encode "[]", not "null", so
// web/src/pages/Nodes.tsx's array methods (Array.prototype.reduce et al)
// never have to defend against a null response body.
func TestNodeUsageEmptyClusterReturnsEmptyArray(t *testing.T) {
	fk := newFakeKube()
	s := newTestServer(t, fk, "")
	h := s.Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/usage", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("expected an empty JSON array body, got %q", got)
	}
}
