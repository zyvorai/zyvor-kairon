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

// TestMachineTimelineMergesEventsAndConditions checks both sources appear
// in one list, newest first.
func TestMachineTimelineMergesEventsAndConditions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fk := newFakeKube()
	fk.machines["vm-1"] = model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "default"},
		Status: model.MachineStatus{Conditions: []model.Condition{
			{Type: "Ready", Status: "True", Reason: "FluxVMReconciled", LastTransitionTime: now.Add(-time.Minute)},
		}},
	}
	fk.events = []model.Event{
		{Reason: "Scheduled", Message: "placed on worker-1", Type: "Normal", LastTimestamp: now.Add(-2 * time.Minute), Count: 1},
		{Reason: "Unschedulable", Message: "no capacity", Type: "Warning", LastTimestamp: now, Count: 3},
	}
	h := newTestServer(t, fk, "").Handler()

	rr := doJSON(t, h, http.MethodGet, "/api/v1/machines/default/vm-1/timeline", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got []timelineEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %+v", got)
	}
	wantReasons := []string{"Unschedulable", "FluxVMReconciled", "Scheduled"}
	for i, w := range wantReasons {
		if got[i].Reason != w {
			t.Errorf("entry %d reason = %q, want %q", i, got[i].Reason, w)
		}
	}
	if got[0].Source != "event" || got[1].Source != "condition" {
		t.Errorf("sources = %q,%q", got[0].Source, got[1].Source)
	}
}
