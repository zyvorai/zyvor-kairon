// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func findInsight(list []insight, id string) (insight, bool) {
	for _, i := range list {
		if i.ID == id {
			return i, true
		}
	}
	return insight{}, false
}

func TestComputeInsightsRules(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	failed := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Failed", Message: "boom"}}
	stuck := model.Machine{
		Metadata: model.ObjectMeta{Name: "web", Namespace: "prod"},
		Status: model.MachineStatus{Conditions: []model.Condition{
			{Type: "Scheduled", Status: "False", Reason: "Unschedulable", Message: "no node fits", LastTransitionTime: now.Add(-10 * time.Minute)},
		}},
	}
	fresh := model.Machine{
		Metadata: model.ObjectMeta{Name: "new", Namespace: "prod"},
		Status: model.MachineStatus{Conditions: []model.Condition{
			{Type: "Scheduled", Status: "False", Reason: "Unschedulable", LastTransitionTime: now.Add(-time.Minute)},
		}},
	}
	packed := model.Machine{Metadata: model.ObjectMeta{Name: "big", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "n1", PowerState: "Running"}}
	packed.Spec.Resources.CPU = "15"
	packed.Spec.Resources.Memory = "1Gi"

	max := 10
	in := insightInput{
		Machines: []model.Machine{failed, stuck, fresh, packed},
		Migrations: []model.MachineMigration{
			{Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod"}, Status: model.MachineMigrationStatus{Phase: "NeedsRecovery", Message: "operator action"}},
			{Metadata: model.ObjectMeta{Name: "m2", Namespace: "prod", CreationTimestamp: now.Add(-time.Hour)}, Status: model.MachineMigrationStatus{Phase: "Transferring"}},
			{Metadata: model.ObjectMeta{Name: "m3", Namespace: "prod", CreationTimestamp: now.Add(-time.Hour)}, Status: model.MachineMigrationStatus{Phase: "Succeeded"}},
		},
		Nodes: []model.Node{
			{Metadata: model.ObjectMeta{Name: "n1"}, Status: model.NodeStatus{
				Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}}, Allocatable: map[string]string{"cpu": "16", "memory": "64Gi"}}},
			{Metadata: model.ObjectMeta{Name: "n2"}, Status: model.NodeStatus{Conditions: []model.NodeCondition{{Type: "Ready", Status: "False"}}}},
		},
		Quotas: []model.MachineQuota{{
			Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
			Spec:     model.MachineQuotaSpec{MaxMachines: &max},
			Status:   model.MachineQuotaStatus{UsedMachines: 9},
		}},
	}
	got := computeInsights(in, now)

	for _, id := range []string{
		"machine-failed/prod/db", "unschedulable/prod/web", "migration-recovery/prod/m1",
		"migration-stuck/prod/m2", "node-packed/n1", "quota-near/prod/q",
	} {
		if _, ok := findInsight(got, id); !ok {
			t.Errorf("missing insight %s; got %+v", id, got)
		}
	}
	for _, id := range []string{"unschedulable/prod/new", "migration-stuck/prod/m3", "node-not-ready/n2"} {
		if _, ok := findInsight(got, id); ok {
			t.Errorf("unexpected insight %s", id)
		}
	}
	if len(got) == 0 || got[0].Severity != sevCritical {
		t.Errorf("criticals should sort first: %+v", got)
	}
}

func TestNodeNotReadyOnlyWithMachines(t *testing.T) {
	now := time.Now()
	m := model.Machine{Metadata: model.ObjectMeta{Name: "a", Namespace: "d"}, Spec: model.MachineSpec{NodeName: "n2", PowerState: "Running"}}
	n := model.Node{Metadata: model.ObjectMeta{Name: "n2"}, Status: model.NodeStatus{Conditions: []model.NodeCondition{{Type: "Ready", Status: "False"}}}}
	if _, ok := findInsight(computeInsights(insightInput{Machines: []model.Machine{m}, Nodes: []model.Node{n}}, now), "node-not-ready/n2"); !ok {
		t.Error("node with machines and not Ready should be flagged")
	}
}

func TestInsightsEndpointReturnsEmptyArray(t *testing.T) {
	h := newTestServer(t, newFakeKube(), "").Handler()
	rr := doJSON(t, h, http.MethodGet, "/api/v1/insights", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got []insight
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil || got == nil {
		t.Fatalf("want empty JSON array, got %q (%v)", rr.Body.String(), err)
	}
}
