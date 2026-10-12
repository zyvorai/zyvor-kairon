// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

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
	"github.com/zyvorai/kairon/internal/scheduler"
)

type resumeFake struct {
	mu          sync.Mutex
	movedTo     string
	hibernation *model.HibernationStatus
}

func newResumeController(t *testing.T) (*Controller, *resumeFake) {
	t.Helper()
	fake := &resumeFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1":
			var p struct {
				Spec struct {
					NodeName string `json:"nodeName"`
				} `json:"spec"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			fake.movedTo = p.Spec.NodeName
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/vm-1/status":
			var p struct {
				Status struct {
					Hibernation *model.HibernationStatus `json:"hibernation"`
				} `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			fake.hibernation = p.Status.Hibernation
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
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

func hostFacts() model.HibernationHostFacts {
	return model.HibernationHostFacts{QEMUVersion: "10.2.1", MachineType: "pc-q35-10.2", CPUVendor: "GenuineIntel", CPUFlags: []string{"avx2", "sse4_2"}, StateDir: "/mnt/shared/hibernation"}
}

func readyNode(name, memory string, ready bool) model.Node {
	var n model.Node
	n.Metadata.Name = name
	status := "True"
	if !ready {
		status = "False"
	}
	n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: status}}
	n.Status.Allocatable = map[string]string{"cpu": "8", "memory": memory}
	return n
}

func hibernatedMachine(node string, shared bool) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "vm-1", Namespace: "prod"},
		Spec:     model.MachineSpec{NodeName: node, PowerState: "Running"},
	}
	m.Spec.Resources.CPU = "2"
	m.Spec.Resources.Memory = "4Gi"
	m.Status.Phase = model.PowerStateHibernated
	m.Status.Hibernation = &model.HibernationStatus{Node: node, RuntimeID: "rt-1", SharedStorage: shared, Host: hostFacts()}
	return m
}

func topologyWith(node string, facts *model.HibernationHostFacts) model.NodeTopology {
	var t model.NodeTopology
	t.Metadata.Name = node
	t.Status.Hibernation = facts
	return t
}

func TestHibernatedResumesOnItsOwnNode(t *testing.T) {
	ctl, fake := newResumeController(t)
	m := hibernatedMachine("a", true)
	ctl.reconcileHibernatedResumes(context.Background(), []model.Machine{m}, []model.Node{readyNode("a", "16Gi", true)}, map[string]scheduler.NodeLoad{}, nil)
	if fake.movedTo != "" {
		t.Fatalf("moved to %s", fake.movedTo)
	}
	if fake.hibernation == nil || fake.hibernation.ResumeNode != "a" {
		t.Fatalf("resume node %+v", fake.hibernation)
	}
}

func TestHibernatedMovesToCompatibleNodeWhenOwnIsDown(t *testing.T) {
	ctl, fake := newResumeController(t)
	m := hibernatedMachine("a", true)
	f := hostFacts()
	f.CPUFlags = append(f.CPUFlags, "avx512f")
	old := hostFacts()
	old.QEMUVersion = "9.2.0"
	topologies := map[string]model.NodeTopology{
		"b": topologyWith("b", &old),
		"c": topologyWith("c", &f),
	}
	nodes := []model.Node{readyNode("a", "16Gi", false), readyNode("b", "16Gi", true), readyNode("c", "16Gi", true)}
	ctl.reconcileHibernatedResumes(context.Background(), []model.Machine{m}, nodes, map[string]scheduler.NodeLoad{}, topologies)
	if fake.movedTo != "c" {
		t.Fatalf("want move to c (b runs another QEMU), got %q", fake.movedTo)
	}
	if fake.hibernation == nil || fake.hibernation.ResumeNode != "c" {
		t.Fatalf("resume node %+v", fake.hibernation)
	}
}

func TestHibernatedNodeLocalStateWaitsForItsNode(t *testing.T) {
	ctl, fake := newResumeController(t)
	m := hibernatedMachine("a", false)
	f := hostFacts()
	nodes := []model.Node{readyNode("a", "16Gi", false), readyNode("b", "16Gi", true)}
	ctl.reconcileHibernatedResumes(context.Background(), []model.Machine{m}, nodes, map[string]scheduler.NodeLoad{}, map[string]model.NodeTopology{"b": topologyWith("b", &f)})
	if fake.movedTo != "" {
		t.Fatalf("moved node-local state to %s", fake.movedTo)
	}
	if fake.hibernation == nil || fake.hibernation.ResumeNode != "" || !strings.Contains(fake.hibernation.Message, "node-local") {
		t.Fatalf("want a waiting message, got %+v", fake.hibernation)
	}
}

func TestHibernatedFullNodeAndNoCompatibleNodeWaits(t *testing.T) {
	ctl, fake := newResumeController(t)
	m := hibernatedMachine("a", true)
	nodes := []model.Node{readyNode("a", "2Gi", true), readyNode("b", "16Gi", true)}
	ctl.reconcileHibernatedResumes(context.Background(), []model.Machine{m}, nodes, map[string]scheduler.NodeLoad{}, nil)
	if fake.movedTo != "" {
		t.Fatalf("moved to %s", fake.movedTo)
	}
	if fake.hibernation == nil || !strings.Contains(fake.hibernation.Message, "b: no hibernation facts published") {
		t.Fatalf("want reasons per node, got %+v", fake.hibernation)
	}
}

func TestAwaitingResume(t *testing.T) {
	m := hibernatedMachine("a", true)
	if !awaitingResume(m) {
		t.Fatal("running hibernated machine without a resume node should await one")
	}
	m.Status.Hibernation.ResumeNode = "a"
	if awaitingResume(m) {
		t.Fatal("already has a resume node")
	}
	m = hibernatedMachine("a", true)
	m.Spec.PowerState = model.PowerStateHibernated
	if awaitingResume(m) {
		t.Fatal("still wanted hibernated")
	}
}
