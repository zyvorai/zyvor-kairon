// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func drsCluster() ([]model.Node, []model.Machine) {
	nodes := []model.Node{maintNode("a", nil), maintNode("b", nil)}
	machines := make([]model.Machine, 0, 3)
	for _, name := range []string{"vm1", "vm2", "vm3"} {
		m := maintMachine(name, "a")
		m.Spec.Resources = model.ResourceSpec{CPU: "2", Memory: "4Gi"}
		machines = append(machines, m)
	}
	return nodes, machines
}

func TestDRSOffDoesNothing(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	nodes, machines := drsCluster()
	ctl.reconcileDRS(context.Background(), machines, nodes, nil)
	if len(fake.calls) != 0 {
		t.Fatalf("calls=%+v", fake.calls)
	}
}

func TestDRSRecommendRecordsEventsOncePerPlan(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	ctl.DRS = DRS{Mode: DRSModeRecommend, Interval: time.Nanosecond}
	nodes, machines := drsCluster()

	ctl.reconcileDRS(context.Background(), machines, nodes, nil)
	time.Sleep(time.Millisecond)
	ctl.reconcileDRS(context.Background(), machines, nodes, nil)

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 0 {
		t.Fatalf("recommend mode created migrations: %+v", got)
	}
	events := fake.find(http.MethodPost, "/namespaces/prod/events")
	if len(events) != 1 || dig(events[0].body, "reason") != "DRSRecommendation" {
		t.Fatalf("events=%+v, want one DRSRecommendation for an unchanged plan", events)
	}
}

func TestDRSAutomaticCreatesLabeledMigrationWithTarget(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	ctl.DRS = DRS{Mode: DRSModeAutomatic}
	nodes, machines := drsCluster()

	ctl.reconcileDRS(context.Background(), machines, nodes, nil)

	created := fake.find(http.MethodPost, "/namespaces/prod/machinemigrations")
	if len(created) != 1 {
		t.Fatalf("migrations=%+v", created)
	}
	body := created[0].body
	if dig(body, "spec", "targetNode") != "b" || dig(body, "spec", "strategy") != "live" || dig(body, "metadata", "labels", model.LabelDRSMigration) != "true" {
		t.Fatalf("migration=%+v", body)
	}
	moved := fake.find(http.MethodPatch, "/machines/vm1")
	if len(moved) != 1 || dig(moved[0].body, "metadata", "annotations", model.AnnotationDRSMovedAt) == nil {
		t.Fatalf("machine patches=%+v, want the moved-at cooldown", moved)
	}
}

func TestDRSAutomaticWaitsForPreviousRound(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	ctl.DRS = DRS{Mode: DRSModeAutomatic}
	nodes, machines := drsCluster()
	inFlight := model.MachineMigration{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "drs-old", Labels: map[string]string{model.LabelDRSMigration: "true"}},
		Spec:     model.MachineMigrationSpec{MachineName: "other"},
		Status:   model.MachineMigrationStatus{Phase: "Running"},
	}

	ctl.reconcileDRS(context.Background(), machines, nodes, []model.MachineMigration{inFlight})

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 0 {
		t.Fatalf("migrations=%+v, want none while a DRS migration is in flight", got)
	}
}

func TestDRSAutomaticHonorsDisruptionBudget(t *testing.T) {
	budget := model.MachineDisruptionBudget{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "all"},
		Spec:     model.MachineDisruptionBudgetSpec{MaxUnavailable: "0", Selector: map[string]string{"app": "web"}},
	}
	ctl, fake := newRecordingController(t, []model.MachineDisruptionBudget{budget})
	ctl.DRS = DRS{Mode: DRSModeAutomatic}
	nodes, machines := drsCluster()
	for i := range machines {
		machines[i].Metadata.Labels = map[string]string{"app": "web"}
	}

	ctl.reconcileDRS(context.Background(), machines, nodes, nil)

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 0 {
		t.Fatalf("migrations=%+v, want none (budget allows 0)", got)
	}
}

func TestDRSHonorsInterval(t *testing.T) {
	ctl, fake := newRecordingController(t, nil)
	ctl.DRS = DRS{Mode: DRSModeAutomatic, Interval: time.Hour}
	nodes, machines := drsCluster()

	ctl.reconcileDRS(context.Background(), machines, nodes, nil)
	ctl.reconcileDRS(context.Background(), machines, nodes, nil)

	if got := fake.find(http.MethodPost, "/machinemigrations"); len(got) != 1 {
		t.Fatalf("migrations=%+v, want one round per interval", got)
	}
}

func TestValidateDRSMode(t *testing.T) {
	for _, ok := range []string{"", "off", "recommend", "automatic"} {
		if err := ValidateDRSMode(ok); err != nil {
			t.Fatalf("%q: %v", ok, err)
		}
	}
	if err := ValidateDRSMode("aggressive"); err == nil {
		t.Fatal("want an error for an unknown mode")
	}
}
