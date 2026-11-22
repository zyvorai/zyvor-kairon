// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fencing

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

type fakePatcher struct {
	spec   map[string]any
	status model.MachineStatus
}

func (f *fakePatcher) PatchMachine(_ context.Context, _, _ string, patch map[string]any) error {
	f.spec = patch
	return nil
}

func (f *fakePatcher) PatchMachineStatus(_ context.Context, _, _ string, status model.MachineStatus) error {
	f.status = status
	return nil
}

func TestFence(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Finalizers: []string{"other", model.Finalizer}},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{Phase: "Running", NodeName: "worker-1", RuntimeID: "r1", GuestIP: "10.0.0.5", AppliedVCPUs: 2},
	}
	f := &fakePatcher{}
	if err := Fence(context.Background(), f, m, "StaleEvacuation", "dead"); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(f.spec)
	want := `{"metadata":{"finalizers":["other"],"labels":{"` + model.AssignedNodeLabel + `":null}},"spec":{"nodeName":""}}`
	if string(got) != want {
		t.Fatalf("machine patch = %s, want %s", got, want)
	}
	if f.status.Phase != "" || f.status.NodeName != "" || f.status.RuntimeID != "" || f.status.GuestIP != "" || f.status.AppliedVCPUs != 0 {
		t.Fatalf("runtime status not cleared: %+v", f.status)
	}
	cond, ok := model.FindCondition(f.status.Conditions, model.ConditionFenced)
	if !ok || cond.Status != "True" || cond.Reason != "StaleEvacuation" || cond.Message != "dead" {
		t.Fatalf("Fenced condition = %+v", cond)
	}
}

func TestFenceWithoutFinalizerLeavesFinalizersAlone(t *testing.T) {
	f := &fakePatcher{}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	if err := Fence(context.Background(), f, m, "OperatorAttested", "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.spec["metadata"].(map[string]any)["finalizers"]; ok {
		t.Fatal("finalizers patched although runtime-cleanup was absent")
	}
}
