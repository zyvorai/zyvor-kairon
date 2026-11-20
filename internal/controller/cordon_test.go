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
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// cordonTestServer is a minimal fake apiserver serving exactly what
// reconcileCordonEvacuation calls: listing MachineDisruptionBudgets,
// creating a MachineMigration, and patching a Machine's annotations.
// Mirrors newWebhookTestController's own inline-httptest-server
// convention rather than introducing a new test-double abstraction.
type cordonTestServer struct {
	mu               sync.Mutex
	budgets          []model.MachineDisruptionBudget
	createdMigration *model.MachineMigration
	patchedAnnos     map[string]string
}

func newCordonTestController(t *testing.T, budgets []model.MachineDisruptionBudget) (*Controller, *cordonTestServer) {
	t.Helper()
	fake := &cordonTestServer{budgets: budgets}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinedisruptionbudgets":
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: fake.budgets})
		case r.Method == http.MethodPost && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machinemigrations":
			var m model.MachineMigration
			_ = json.NewDecoder(r.Body).Decode(&m)
			fake.createdMigration = &m
			_ = json.NewEncoder(w).Encode(m)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1":
			var p struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			fake.patchedAnnos = p.Metadata.Annotations
			w.WriteHeader(http.StatusOK)
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

func cordonedNode(name string) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Spec.Unschedulable = true
	return n
}

func machineOn(node string) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: node, PowerState: "Running"},
	}
}

func TestReconcileCordonEvacuationCreatesMigrationForCordonedNode(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true, Strategy: "cold"}

	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{cordonedNode("worker-1")}, nil)

	if fake.createdMigration == nil {
		t.Fatal("expected a MachineMigration to be created")
	}
	if fake.createdMigration.Spec.MachineName != "vm-1" || fake.createdMigration.Spec.Strategy != "cold" {
		t.Fatalf("unexpected migration spec: %+v", fake.createdMigration.Spec)
	}
	if fake.patchedAnnos[model.AnnotationCordonEvacuateAttemptedAt] == "" {
		t.Fatal("expected the cordon-evacuate-attempted-at annotation to be patched")
	}
}

func TestReconcileCordonEvacuationDisabledCreatesNothing(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	// CordonEvacuation zero value: Enabled defaults to false.

	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{cordonedNode("worker-1")}, nil)

	if fake.createdMigration != nil {
		t.Fatal("expected no MachineMigration to be created while disabled")
	}
}

func TestReconcileCordonEvacuationIgnoresUncordonedNode(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}

	var n model.Node
	n.Metadata.Name = "worker-1" // Unschedulable left false
	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{n}, nil)

	if fake.createdMigration != nil {
		t.Fatal("expected no MachineMigration for a node that isn't cordoned")
	}
}

func TestReconcileCordonEvacuationBlockedByDisruptionBudgetRecordsCooldownOnly(t *testing.T) {
	budget := model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "b"},
		Spec:     model.MachineDisruptionBudgetSpec{MaxUnavailable: "0", Selector: map[string]string{"app": "web"}},
	}
	ctl, fake := newCordonTestController(t, []model.MachineDisruptionBudget{budget})
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}

	machine := machineOn("worker-1")
	machine.Metadata.Labels = map[string]string{"app": "web"}
	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machine}, []model.Node{cordonedNode("worker-1")}, nil)

	if fake.createdMigration != nil {
		t.Fatal("expected no MachineMigration when the disruption budget blocks it")
	}
	if fake.patchedAnnos[model.AnnotationCordonEvacuateAttemptedAt] == "" {
		t.Fatal("expected the cooldown annotation to still be patched when blocked")
	}
}

func TestReconcileCordonEvacuationSkipsAlreadyInFlightMachine(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}

	inFlight := model.MachineMigration{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "existing"},
		Spec:     model.MachineMigrationSpec{MachineName: "vm-1"},
		Status:   model.MachineMigrationStatus{Phase: "Running"},
	}
	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{cordonedNode("worker-1")}, []model.MachineMigration{inFlight})

	if fake.createdMigration != nil {
		t.Fatal("expected no duplicate MachineMigration for a Machine already mid-migration")
	}
}

func TestReconcileCordonEvacuationRetriesAfterCancelledMigration(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}

	cancelled := model.MachineMigration{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "existing"},
		Spec:     model.MachineMigrationSpec{MachineName: "vm-1"},
		Status:   model.MachineMigrationStatus{Phase: "Cancelled"},
	}
	// Cancelled is a real terminal phase (cordonEvacuateTerminal), same as
	// Succeeded/Failed/Blocked -- a Machine whose only migration attempt was
	// operator-cancelled must be retried on the next cordon-evacuation tick,
	// not left stranded on its cordoned node because a stale migration
	// object still looks "in flight".
	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machineOn("worker-1")}, []model.Node{cordonedNode("worker-1")}, []model.MachineMigration{cancelled})

	if fake.createdMigration == nil {
		t.Fatal("expected a new MachineMigration to be created after the prior attempt was Cancelled")
	}
}

func TestReconcileCordonEvacuationHonorsCooldown(t *testing.T) {
	ctl, fake := newCordonTestController(t, nil)
	ctl.CordonEvacuation = CordonEvacuation{Enabled: true}

	machine := machineOn("worker-1")
	machine.Metadata.Annotations = map[string]string{
		model.AnnotationCordonEvacuateAttemptedAt: time.Now().UTC().Format(time.RFC3339),
	}
	ctl.reconcileCordonEvacuation(context.Background(), []model.Machine{machine}, []model.Node{cordonedNode("worker-1")}, nil)

	if fake.createdMigration != nil {
		t.Fatal("expected the cooldown to suppress a new attempt")
	}
}
