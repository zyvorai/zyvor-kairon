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
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestDetectUnreachableNodesSetsConditionWhenNodeMissing(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	var patchedStatus model.MachineStatus
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var p struct {
			Status model.MachineStatus `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		patchedStatus = p.Status
		patched = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, nil)

	if !patched {
		t.Fatal("expected a status patch when the machine's node no longer exists")
	}
	cond, found := model.FindCondition(patchedStatus.Conditions, model.ConditionNodeUnreachable)
	if !found || cond.Status != "True" {
		t.Fatalf("got conditions %+v, want %s=True", patchedStatus.Conditions, model.ConditionNodeUnreachable)
	}
}

func TestDetectUnreachableNodesClearsConditionOnceNodeIsReadyAgain(t *testing.T) {
	machine := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
		Status: model.MachineStatus{Conditions: []model.Condition{
			{Type: model.ConditionNodeUnreachable, Status: "True", Reason: "NodeNotReadyOrMissing"},
		}},
	}
	node := readyCapableNode("worker-1")
	var patchedStatus model.MachineStatus
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		var p struct {
			Status model.MachineStatus `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		patchedStatus = p.Status
		patched = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, []model.Node{node})

	if !patched {
		t.Fatal("expected a status patch clearing the condition once the node is Ready again")
	}
	cond, found := model.FindCondition(patchedStatus.Conditions, model.ConditionNodeUnreachable)
	if !found || cond.Status != "False" {
		t.Fatalf("got conditions %+v, want %s=False", patchedStatus.Conditions, model.ConditionNodeUnreachable)
	}
}

func TestDetectUnreachableNodesNoopsWhenConditionAlreadyCorrect(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	node := readyCapableNode("worker-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, []model.Node{node})
}

func TestDetectUnreachableNodesSkipsMachinesBeingDeletedOrUnscheduled(t *testing.T) {
	now := time.Now().UTC()
	deleting := model.Machine{
		Metadata: model.ObjectMeta{Name: "deleting", Namespace: "prod", DeletionTimestamp: &now},
		Spec:     model.MachineSpec{NodeName: "worker-1"},
	}
	unscheduled := model.Machine{Metadata: model.ObjectMeta{Name: "unscheduled", Namespace: "prod"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	c.detectUnreachableNodes(context.Background(), []model.Machine{deleting, unscheduled}, nil)
}

func TestDetectUnreachableNodesStaleLivenessLeaseMarksReadyNodeUnreachable(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	node := readyCapableNode("worker-1")
	stale := model.NewMicroTime(time.Now().Add(-10 * time.Minute))
	dur := int32(60)
	var patchedStatus model.MachineStatus
	patched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/coordination.k8s.io/v1/namespaces/kairon-system/leases/kairon-node-worker-1":
			_ = json.NewEncoder(w).Encode(model.Lease{
				Metadata: model.ObjectMeta{Name: "kairon-node-worker-1", Namespace: "kairon-system"},
				Spec:     model.LeaseSpec{RenewTime: &stale, LeaseDurationSeconds: &dur},
			})
		case r.Method == http.MethodPatch:
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			patchedStatus = p.Status
			patched = true
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{
		Kube:                       kc,
		Log:                        slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeLivenessLeaseNamespace: "kairon-system",
	}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, []model.Node{node})

	if !patched {
		t.Fatal("expected a status patch when Ready node has a stale kairon-node liveness Lease")
	}
	cond, found := model.FindCondition(patchedStatus.Conditions, model.ConditionNodeUnreachable)
	if !found || cond.Status != "True" || cond.Reason != "AgentLivenessStale" {
		t.Fatalf("got conditions %+v, want %s=True reason=AgentLivenessStale", patchedStatus.Conditions, model.ConditionNodeUnreachable)
	}
}

func TestDetectUnreachableNodesFreshLivenessLeaseLeavesReadyNodeAlone(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	node := readyCapableNode("worker-1")
	fresh := model.NewMicroTime(time.Now().Add(-1 * time.Second))
	dur := int32(60)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apis/coordination.k8s.io/v1/namespaces/kairon-system/leases/kairon-node-worker-1" {
			_ = json.NewEncoder(w).Encode(model.Lease{
				Metadata: model.ObjectMeta{Name: "kairon-node-worker-1", Namespace: "kairon-system"},
				Spec:     model.LeaseSpec{RenewTime: &fresh, LeaseDurationSeconds: &dur},
			})
			return
		}
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{
		Kube:                       kc,
		Log:                        slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeLivenessLeaseNamespace: "kairon-system",
	}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, []model.Node{node})
}

func TestDetectUnreachableNodesLeaseLookupErrorFailsOpen(t *testing.T) {
	machine := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}, Spec: model.MachineSpec{NodeName: "worker-1"}}
	node := readyCapableNode("worker-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{
		Kube:                       kc,
		Log:                        slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeLivenessLeaseNamespace: "kairon-system",
	}

	c.detectUnreachableNodes(context.Background(), []model.Machine{machine}, []model.Node{node})
}
