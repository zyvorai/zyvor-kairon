// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestNodeCapacityWhatIf(t *testing.T) {
	fk := newFakeKube()
	ready := []model.NodeCondition{{Type: "Ready", Status: "True"}}
	fk.nodes = []model.Node{
		{Metadata: model.ObjectMeta{Name: "big"}, Status: model.NodeStatus{Conditions: ready, Allocatable: map[string]string{"cpu": "16", "memory": "64Gi"}}},
		{Metadata: model.ObjectMeta{Name: "small"}, Status: model.NodeStatus{Conditions: ready, Allocatable: map[string]string{"cpu": "4", "memory": "8Gi"}}},
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec:     model.MachineSpec{NodeName: "small", PowerState: "Running"},
	}
	m.Spec.Resources.CPU = "3"
	m.Spec.Resources.Memory = "4Gi"
	fk.machines["vm"] = m
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/capacity?cpu=2&memory=2Gi", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var rows []nodeCapacity
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Node != "big" || rows[1].Node != "small" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[1].ReqCPUMilli != 3000 || rows[1].ReqMemoryMiB != 4096 || rows[1].Machines != 1 {
		t.Errorf("small requested = %+v", rows[1])
	}
	if !rows[0].WhatIf.Fits {
		t.Errorf("big should fit: %+v", rows[0].WhatIf)
	}
	if rows[1].WhatIf.Fits || rows[1].WhatIf.Reason == "" {
		t.Errorf("small should not fit a 2 vCPU VM with 3 of 4 used: %+v", rows[1].WhatIf)
	}

	if rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/capacity?cpu=banana&memory=1Gi", "", nil); rr.Code != http.StatusBadRequest {
		t.Errorf("bad cpu: status %d", rr.Code)
	}
}
