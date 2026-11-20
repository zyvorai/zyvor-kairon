// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
)

func webMachine(name, node, phase string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Labels: map[string]string{"tier": "web"}},
		Spec:     model.MachineSpec{NodeName: node},
		Status:   model.MachineStatus{Phase: phase},
	}
}

func webBudget(name, minAvailable string) model.MachineDisruptionBudget {
	return model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: map[string]string{"tier": "web"}, MinAvailable: minAvailable},
	}
}

func TestAdmitDisruptionAllowsUntilBudgetExhausted(t *testing.T) {
	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
		webMachine("web-3", "node-a", "Running"),
	}
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{webBudget("web-pdb", "2")}, machines, nil)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	// 3 healthy, minAvailable 2 -> exactly 1 disruption allowed.
	if reason := AdmitDisruption(states, machines[0]); reason != "" {
		t.Fatalf("first disruption should be admitted, got reason %q", reason)
	}
	if reason := AdmitDisruption(states, machines[1]); reason == "" {
		t.Fatal("second disruption should be blocked once the budget is spent")
	}
}

func TestAdmitDisruptionIgnoresMachinesAlreadyMidMigration(t *testing.T) {
	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
	}
	migrations := []model.MachineMigration{
		{Metadata: model.ObjectMeta{Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}, Status: model.MachineMigrationStatus{Phase: "Starting"}},
	}
	// minAvailable 2, but web-1 is already mid-migration -> only web-2 counts
	// as healthy, so disrupting it too would go below minAvailable.
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{webBudget("web-pdb", "2")}, machines, migrations)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	if reason := AdmitDisruption(states, machines[1]); reason == "" {
		t.Fatal("expected the in-flight migration to count against currentHealthy, blocking this disruption")
	}
}

func TestAdmitDisruptionTreatsCancelledMigrationAsTerminal(t *testing.T) {
	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
	}
	migrations := []model.MachineMigration{
		{Metadata: model.ObjectMeta{Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}, Status: model.MachineMigrationStatus{Phase: "Cancelled"}},
	}
	// minAvailable 1 (not 2, unlike TestAdmitDisruptionIgnoresMachinesAlready
	// MidMigration -- at minAvailable == total there's no daylight between
	// "counts as healthy" and "doesn't", since 0 disruptions are allowed
	// either way): if Cancelled still counted as in-flight, healthy would be
	// 1 and 0 disruptions would be allowed; since it's a real terminal
	// phase, both machines count healthy and one disruption is allowed.
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{webBudget("web-pdb", "1")}, machines, migrations)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	if reason := AdmitDisruption(states, machines[1]); reason != "" {
		t.Fatalf("expected a Cancelled migration to no longer count against currentHealthy, got reason %q", reason)
	}
}

func TestAdmitDisruptionSpendsAllowanceAcrossEveryMatchingBudget(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "both", Namespace: "prod", Labels: map[string]string{"tier": "web", "team": "payments"}},
		Status:   model.MachineStatus{Phase: "Running"},
	}
	tierBudget := model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Name: "tier-pdb", Namespace: "prod"},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: map[string]string{"tier": "web"}, MaxUnavailable: "1"},
	}
	teamBudget := model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Name: "team-pdb", Namespace: "prod"},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: map[string]string{"team": "payments"}, MaxUnavailable: "0"},
	}
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{tierBudget, teamBudget}, []model.Machine{m}, nil)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	// tierBudget alone would allow 1 disruption, but teamBudget (maxUnavailable
	// 0 out of the same 1 total machine) allows none -- the stricter budget wins.
	if reason := AdmitDisruption(states, m); reason == "" {
		t.Fatal("expected the stricter team budget to block this disruption")
	}
}

func TestAdmitDisruptionMachineNotMatchingAnyBudgetIsAlwaysAllowed(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "unmanaged", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Running"}}
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{webBudget("web-pdb", "100")}, []model.Machine{m}, nil)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	if reason := AdmitDisruption(states, m); reason != "" {
		t.Fatalf("expected no budget to apply, got reason %q", reason)
	}
}

func TestBudgetStateStatusReportsRealNumbers(t *testing.T) {
	machines := []model.Machine{
		webMachine("web-1", "node-a", "Running"),
		webMachine("web-2", "node-a", "Running"),
		webMachine("web-3", "node-a", "Running"),
	}
	migrations := []model.MachineMigration{
		{Metadata: model.ObjectMeta{Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "web-1"}, Status: model.MachineMigrationStatus{Phase: "Starting"}},
	}
	states, err := LoadBudgetStates([]model.MachineDisruptionBudget{webBudget("web-pdb", "2")}, machines, migrations)
	if err != nil {
		t.Fatalf("LoadBudgetStates: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("len(states) = %d, want 1", len(states))
	}
	got := states[0].Status()
	// 3 machines match; web-1 is mid-migration so only web-2/web-3 count as
	// healthy (2); minAvailable=2 desired; disruptionsAllowed = 2-2 = 0.
	want := model.MachineDisruptionBudgetStatus{ExpectedMachines: 3, CurrentHealthy: 2, DesiredHealthy: 2, DisruptionsAllowed: 0}
	if got != want {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}

func TestReconcileDisruptionBudgetsStatusPatchesRealStatus(t *testing.T) {
	var patchedBody map[string]model.MachineDisruptionBudgetStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinedisruptionbudgets/web-pdb/status" {
			_ = json.NewDecoder(r.Body).Decode(&patchedBody)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets" {
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: []model.MachineDisruptionBudget{webBudget("web-pdb", "1")}})
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running"), webMachine("web-2", "node-a", "Running")}
	if err := ctl.reconcileDisruptionBudgetsStatus(context.Background(), machines, nil); err != nil {
		t.Fatalf("reconcileDisruptionBudgetsStatus: %v", err)
	}
	got, ok := patchedBody["status"]
	if !ok {
		t.Fatal("expected a status patch to have been sent")
	}
	want := model.MachineDisruptionBudgetStatus{ExpectedMachines: 2, CurrentHealthy: 2, DesiredHealthy: 1, DisruptionsAllowed: 1}
	if got != want {
		t.Fatalf("patched status = %+v, want %+v", got, want)
	}
}

// TestReconcileDisruptionBudgetsStatusObservesMetrics confirms
// reconcileDisruptionBudgetsStatus wires Metrics.ObserveDisruptionBudgets
// into the same tick that patches MachineDisruptionBudget status, and that
// what lands in kairon_disruption_budget_status matches what got patched --
// not a second, independently-computed tally that could drift from it.
// Mirrors TestReconcileObservesQuotaMetrics in controller_test.go.
func TestReconcileDisruptionBudgetsStatusObservesMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinedisruptionbudgets/web-pdb/status" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets" {
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: []model.MachineDisruptionBudget{webBudget("web-pdb", "1")}})
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	kc.HTTP = srv.Client()
	rec := metrics.NewRecorder()
	ctl := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: rec}

	machines := []model.Machine{webMachine("web-1", "node-a", "Running"), webMachine("web-2", "node-a", "Running")}
	if err := ctl.reconcileDisruptionBudgetsStatus(context.Background(), machines, nil); err != nil {
		t.Fatalf("reconcileDisruptionBudgetsStatus: %v", err)
	}

	rr := httptest.NewRecorder()
	rec.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`kairon_disruption_budget_status{budget="web-pdb",field="expected_machines",namespace="prod"} 2`,
		`kairon_disruption_budget_status{budget="web-pdb",field="current_healthy",namespace="prod"} 2`,
		`kairon_disruption_budget_status{budget="web-pdb",field="desired_healthy",namespace="prod"} 1`,
		`kairon_disruption_budget_status{budget="web-pdb",field="disruptions_allowed",namespace="prod"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
