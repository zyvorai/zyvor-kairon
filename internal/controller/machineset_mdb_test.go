// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// mdbScaleDownServer is a fake apiserver for MachineSet scale-down gating:
// it serves MachineDisruptionBudgets and MachineMigrations, and records
// Machine deletes, Events and the MachineSet status patch.
type mdbScaleDownServer struct {
	mu      sync.Mutex
	budgets []model.MachineDisruptionBudget
	deleted []string
	events  []model.Event
	patched model.MachineSetStatus
}

func newMDBScaleDownController(t *testing.T, budgets ...model.MachineDisruptionBudget) (*Controller, *mdbScaleDownServer) {
	t.Helper()
	fake := &mdbScaleDownServer{budgets: budgets}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets":
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: fake.budgets})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"):
			fake.deleted = append(fake.deleted, strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/prod/events":
			var ev model.Event
			_ = json.NewDecoder(r.Body).Decode(&ev)
			fake.events = append(fake.events, ev)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/ms1/status":
			var body struct {
				Status model.MachineSetStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.patched = body.Status
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinesets/ms1":
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fake
}

// ms1Budget selects every replica of ms1 (they carry LabelMachineSet=ms1).
func ms1Budget(ns, minAvailable, maxUnavailable string) model.MachineDisruptionBudget {
	return model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Name: "web", Namespace: ns},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: map[string]string{model.LabelMachineSet: "ms1"}, MinAvailable: minAvailable, MaxUnavailable: maxUnavailable},
	}
}

// scaleDownFixture is ms1 scaled 3 -> desired, with three current-template
// replicas in the given phases.
func scaleDownFixture(desired int, phases ...string) (model.MachineSet, []model.Machine) {
	ms := testMachineSet(desired, "", "")
	ms.Metadata.Finalizers = []string{model.FinalizerMachineSet}
	hash := machineSetTemplateHash(ms.Spec.Template)
	var owned []model.Machine
	for i, p := range phases {
		owned = append(owned, ownedMachine("ms1-"+string(rune('a'+i)), hash, p))
	}
	return ms, owned
}

func scaleDownBlockedCondition(t *testing.T, st model.MachineSetStatus) model.Condition {
	t.Helper()
	c, ok := model.FindCondition(st.Conditions, machineSetConditionScaleDownBlocked)
	if !ok {
		t.Fatalf("status.conditions has no %s: %+v", machineSetConditionScaleDownBlocked, st.Conditions)
	}
	return c
}

func TestMachineSetScaleDownBlockedByDisruptionBudget(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("prod", "3", ""))
	ms, owned := scaleDownFixture(1, "Running", "Running", "Running")
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 0 {
		t.Fatalf("minAvailable=3 of 3 healthy must block every delete, got %v", fake.deleted)
	}
	c := scaleDownBlockedCondition(t, fake.patched)
	if c.Status != "True" || c.Reason != machineSetReasonDisruptionBudget || !strings.Contains(c.Message, "prod/web") {
		t.Fatalf("want ScaleDownBlocked=True/DisruptionBudget naming prod/web, got %+v", c)
	}
	if fake.patched.Message != "" {
		t.Fatalf("a budget hold-back is not a reconcile error, got message %q", fake.patched.Message)
	}
	if len(fake.events) != 1 || fake.events[0].Reason != machineSetConditionScaleDownBlocked || fake.events[0].Type != "Warning" {
		t.Fatalf("want one Warning ScaleDownBlocked event, got %+v", fake.events)
	}
}

func TestMachineSetScaleDownAllowedByDisruptionBudget(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("prod", "1", ""))
	ms, owned := scaleDownFixture(1, "Running", "Running", "Running")
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 2 {
		t.Fatalf("minAvailable=1 allows 2 disruptions; want 2 deletes, got %v", fake.deleted)
	}
	if c := scaleDownBlockedCondition(t, fake.patched); c.Status != "False" {
		t.Fatalf("want ScaleDownBlocked=False, got %+v", c)
	}
	if len(fake.events) != 0 {
		t.Fatalf("no event expected when nothing is blocked, got %+v", fake.events)
	}
}

func TestMachineSetScaleDownPartiallyAllowedByDisruptionBudget(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("prod", "", "1"))
	ms, owned := scaleDownFixture(1, "Running", "Running", "Running")
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 1 {
		t.Fatalf("maxUnavailable=1 allows exactly 1 of the 2 excess deletes, got %v", fake.deleted)
	}
	c := scaleDownBlockedCondition(t, fake.patched)
	if c.Status != "True" || !strings.Contains(c.Message, "deleted 1 of 2") {
		t.Fatalf("want ScaleDownBlocked=True reporting 1 of 2, got %+v", c)
	}
}

// Not-Running replicas go first and spend no allowance, so a budget with
// zero disruptions left still lets the controller drop a broken replica.
func TestMachineSetScaleDownPrefersUnhealthyReplicas(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("prod", "2", ""))
	ms, owned := scaleDownFixture(2, "Running", "Running", "Failed")
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 1 || fake.deleted[0] != "ms1-c" {
		t.Fatalf("want the Failed replica ms1-c deleted, got %v", fake.deleted)
	}
	if c := scaleDownBlockedCondition(t, fake.patched); c.Status != "False" {
		t.Fatalf("want ScaleDownBlocked=False, got %+v", c)
	}
}

// The Warning Event fires on the transition to blocked, not every tick.
func TestMachineSetScaleDownBlockedEventOnlyOnTransition(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("prod", "3", ""))
	ms, owned := scaleDownFixture(1, "Running", "Running", "Running")
	ms.Status.Conditions = []model.Condition{{Type: machineSetConditionScaleDownBlocked, Status: "True", Reason: machineSetReasonDisruptionBudget}}
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.events) != 0 {
		t.Fatalf("already blocked: want no new event, got %+v", fake.events)
	}
}

// Budgets are namespaced: one in another namespace never gates prod.
func TestMachineSetScaleDownIgnoresBudgetsInOtherNamespaces(t *testing.T) {
	ctl, fake := newMDBScaleDownController(t, ms1Budget("staging", "3", ""))
	ms, owned := scaleDownFixture(1, "Running", "Running", "Running")
	ctl.reconcileMachineSets(context.Background(), []model.MachineSet{ms}, owned)
	if len(fake.deleted) != 2 {
		t.Fatalf("want 2 deletes (budget is in staging), got %v", fake.deleted)
	}
}
