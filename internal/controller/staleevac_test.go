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
	"time"

	"github.com/zyvorai/kairon/internal/fencing"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func staleEvacNode(ready bool, fenced string) model.Node {
	n := readyCapableNode("worker-1")
	if !ready {
		n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "Unknown"}}
	}
	if fenced != "" {
		n.Metadata.Annotations = map[string]string{fencing.AnnotationNodeFenced: fenced}
	}
	return n
}

func staleEvacMachine(name string, optIn, unreachable bool) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status:   model.MachineStatus{NodeName: "worker-1", Phase: "Running"},
	}
	if optIn {
		m.Metadata.Annotations = map[string]string{fencing.AnnotationEvacuate: "true"}
	}
	if unreachable {
		m.Status.Conditions = []model.Condition{{Type: model.ConditionNodeUnreachable, Status: "True", Reason: "NodeNotReadyOrMissing"}}
	}
	return m
}

type staleEvacRecorder struct {
	mu      sync.Mutex
	patched map[string]int
	lease   func(w http.ResponseWriter)
	budgets []model.MachineDisruptionBudget
}

func (r *staleEvacRecorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.patched[name]
}

func newStaleEvacController(t *testing.T, rec *staleEvacRecorder, se StaleEvacuation, leaseNS string) *Controller {
	t.Helper()
	rec.patched = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/machinedisruptionbudgets"):
			_ = json.NewEncoder(w).Encode(model.MachineDisruptionBudgetList{Items: rec.budgets})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/leases/"):
			if rec.lease == nil {
				http.Error(w, `{"kind":"Status","code":404}`, http.StatusNotFound)
				return
			}
			rec.lease(w)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/machines/"):
			name := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/machines/")+len("/machines/"):], "/status")
			rec.mu.Lock()
			rec.patched[name]++
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	return &Controller{
		Kube:                       kc,
		Log:                        slog.New(slog.NewTextHandler(io.Discard, nil)),
		StaleEvacuation:            se,
		NodeLivenessLeaseNamespace: leaseNS,
	}
}

func TestStaleEvacuationFencesOptedInMachineOnAttestedDeadNode(t *testing.T) {
	rec := &staleEvacRecorder{}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "")
	c.reconcileStaleEvacuation(context.Background(),
		[]model.Machine{staleEvacMachine("db", true, true)},
		[]model.Node{staleEvacNode(false, "ipmi power-off confirmed")}, nil)
	if rec.count("db") == 0 {
		t.Fatal("expected the opted-in machine to be fenced")
	}
}

func TestStaleEvacuationGates(t *testing.T) {
	now := time.Now().UTC()
	deleting := staleEvacMachine("db", true, true)
	deleting.Metadata.DeletionTimestamp = &now
	cases := []struct {
		name       string
		se         StaleEvacuation
		machine    model.Machine
		node       *model.Node
		migrations []model.MachineMigration
	}{
		{name: "disabled", se: StaleEvacuation{}, machine: staleEvacMachine("db", true, true)},
		{name: "not opted in", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", false, true)},
		{name: "no NodeUnreachable condition", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, false)},
		{name: "deleting", se: StaleEvacuation{Enabled: true}, machine: deleting},
		{name: "node not annotated", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, true), node: ptr(staleEvacNode(false, ""))},
		{name: "blank annotation", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, true), node: ptr(staleEvacNode(false, "  "))},
		{name: "node Ready", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, true), node: ptr(staleEvacNode(true, "dead"))},
		{name: "node object missing", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, true), node: &model.Node{}},
		{
			name: "migration in flight", se: StaleEvacuation{Enabled: true}, machine: staleEvacMachine("db", true, true),
			migrations: []model.MachineMigration{{Metadata: model.ObjectMeta{Name: "mv", Namespace: "prod"}, Spec: model.MachineMigrationSpec{MachineName: "db"}, Status: model.MachineMigrationStatus{Phase: "Copying"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &staleEvacRecorder{}
			c := newStaleEvacController(t, rec, tc.se, "")
			node := staleEvacNode(false, "dead")
			if tc.node != nil {
				node = *tc.node
			}
			c.reconcileStaleEvacuation(context.Background(), []model.Machine{tc.machine}, []model.Node{node}, tc.migrations)
			if rec.count("db") != 0 {
				t.Fatalf("machine was fenced; want gate %q to refuse", tc.name)
			}
		})
	}
}

func staleLease(age time.Duration) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"metadata":{"name":"kairon-node-worker-1","namespace":"kairon-system"},"spec":{"renewTime":"`+
			time.Now().Add(-age).UTC().Format("2006-01-02T15:04:05.000000Z")+`","leaseDurationSeconds":60}}`)
	}
}

func TestStaleEvacuationReadyNodeWithStaleLeaseFences(t *testing.T) {
	rec := &staleEvacRecorder{lease: staleLease(10 * time.Minute)}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "kairon-system")
	c.reconcileStaleEvacuation(context.Background(),
		[]model.Machine{staleEvacMachine("db", true, true)},
		[]model.Node{staleEvacNode(true, "dead")}, nil)
	if rec.count("db") == 0 {
		t.Fatal("a stale kairon-node lease marks a Ready node unreachable; want the machine fenced")
	}
}

func TestStaleEvacuationDisruptionBudgetBlocks(t *testing.T) {
	rec := &staleEvacRecorder{budgets: []model.MachineDisruptionBudget{{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineDisruptionBudgetSpec{Selector: map[string]string{"app": "db"}, MaxUnavailable: "0"},
	}}}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "")
	m := staleEvacMachine("db", true, true)
	m.Metadata.Labels = map[string]string{"app": "db"}
	c.reconcileStaleEvacuation(context.Background(), []model.Machine{m}, []model.Node{staleEvacNode(false, "dead")}, nil)
	if rec.count("db") != 0 {
		t.Fatal("a budget with 0 disruptions allowed must block stale evacuation")
	}
}

func TestStaleEvacuationFreshLeaseRefuses(t *testing.T) {
	rec := &staleEvacRecorder{lease: staleLease(0)}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "kairon-system")
	c.reconcileStaleEvacuation(context.Background(),
		[]model.Machine{staleEvacMachine("db", true, true)},
		[]model.Node{staleEvacNode(false, "dead")}, nil)
	if rec.count("db") != 0 {
		t.Fatal("a fresh kairon-node lease must block stale evacuation")
	}
}

func TestStaleEvacuationLeaseErrorRefuses(t *testing.T) {
	rec := &staleEvacRecorder{lease: func(w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "kairon-system")
	c.reconcileStaleEvacuation(context.Background(),
		[]model.Machine{staleEvacMachine("db", true, true)},
		[]model.Node{staleEvacNode(false, "dead")}, nil)
	if rec.count("db") != 0 {
		t.Fatal("a lease lookup error must fail closed")
	}
}

func TestStaleEvacuationMissingLeaseAllows(t *testing.T) {
	rec := &staleEvacRecorder{}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true}, "kairon-system")
	c.reconcileStaleEvacuation(context.Background(),
		[]model.Machine{staleEvacMachine("db", true, true)},
		[]model.Node{staleEvacNode(false, "dead")}, nil)
	if rec.count("db") == 0 {
		t.Fatal("a missing lease should not block an attested-dead node")
	}
}

func TestStaleEvacuationMaxPerTick(t *testing.T) {
	rec := &staleEvacRecorder{}
	c := newStaleEvacController(t, rec, StaleEvacuation{Enabled: true, MaxPerTick: 2}, "")
	machines := []model.Machine{
		staleEvacMachine("a", true, true),
		staleEvacMachine("b", true, true),
		staleEvacMachine("c", true, true),
	}
	c.reconcileStaleEvacuation(context.Background(), machines, []model.Node{staleEvacNode(false, "dead")}, nil)
	fenced := 0
	for _, n := range []string{"a", "b", "c"} {
		if rec.count(n) > 0 {
			fenced++
		}
	}
	if fenced != 2 {
		t.Fatalf("fenced %d machines, want 2 (MaxPerTick)", fenced)
	}
}

func ptr[T any](v T) *T { return &v }
