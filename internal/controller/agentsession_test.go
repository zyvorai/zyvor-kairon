// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

type sessionFake struct {
	mu      sync.Mutex
	patches map[string]map[string]any
	status  map[string]map[string]any
}

func newSessionTestController(t *testing.T) (*Controller, *sessionFake) {
	t.Helper()
	fake := &sessionFake{patches: map[string]map[string]any{}, status: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/apis/kairon.zyvor.dev/v1/namespaces/prod/agentsessions/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		if name, ok := strings.CutSuffix(path, "/status"); ok {
			fake.status[name], _ = body["status"].(map[string]any)
		} else {
			fake.patches[path] = body
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	kc, err := kube.New(srv.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	kc.HTTP = srv.Client()
	return &Controller{Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, fake
}

func sessionNode(name string, capable bool) model.Node {
	n := readyNode(name, "16Gi", true)
	n.Metadata.Labels = map[string]string{}
	if capable {
		n.Metadata.Labels[model.LabelAgentSessionsCapable] = "true"
	}
	return n
}

func testSession(name string) model.AgentSession {
	return model.AgentSession{
		Metadata: model.ObjectMeta{Name: name, Namespace: "prod", CreationTimestamp: time.Now()},
		Spec:     model.AgentSessionSpec{Resources: model.AgentSessionResources{CPU: "2", Memory: "4Gi"}},
	}
}

func TestReconcileAgentSessionsSchedulesOnCapableNode(t *testing.T) {
	c, fake := newSessionTestController(t)
	mac := sessionNode("mac-1", true)
	mac.Spec.Taints = []model.Taint{{Key: model.TaintVMOnly, Effect: model.TaintEffectNoSchedule}}
	nodes := []model.Node{sessionNode("linux-1", false), mac}
	load := scheduler.BuildNodeLoad(nil)
	c.reconcileAgentSessions(t.Context(), []model.AgentSession{testSession("s1")}, nodes, nil, load, nil)

	p := fake.patches["s1"]
	spec, _ := p["spec"].(map[string]any)
	if spec["nodeName"] != "mac-1" {
		t.Fatalf("patch = %v, want nodeName mac-1", p)
	}
	meta, _ := p["metadata"].(map[string]any)
	if fins, _ := meta["finalizers"].([]any); len(fins) != 1 || fins[0] != model.FinalizerAgentSession {
		t.Fatalf("finalizers = %v", meta["finalizers"])
	}
	if got := load["mac-1"]; got.CPUMilli != 2000 || got.MemoryMiB != 4096 {
		t.Fatalf("load = %+v, want the session reserved", got)
	}
}

func TestReconcileAgentSessionsPendingWithoutCapableNode(t *testing.T) {
	c, fake := newSessionTestController(t)
	c.reconcileAgentSessions(t.Context(), []model.AgentSession{testSession("s1")}, []model.Node{sessionNode("linux-1", false)}, nil, scheduler.BuildNodeLoad(nil), nil)
	st := fake.status["s1"]
	if st["phase"] != model.AgentSessionPending || !strings.Contains(st["message"].(string), model.LabelAgentSessionsCapable) {
		t.Fatalf("status = %v", st)
	}
	if _, patched := fake.patches["s1"]; patched {
		t.Fatal("an unschedulable session must not get a node")
	}
}

func TestReconcileAgentSessionsQuota(t *testing.T) {
	c, fake := newSessionTestController(t)
	maxMachines := 1
	trackers, err := BuildQuotaTrackers([]model.MachineQuota{{
		Metadata: model.ObjectMeta{Name: "q", Namespace: "prod"},
		Spec:     model.MachineQuotaSpec{MaxMachines: &maxMachines},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	running := testSession("old")
	running.Spec.NodeName = "mac-1"
	running.Status.Phase = model.AgentSessionRunning
	seedQuotaWithAgentSessions(trackers, []model.AgentSession{running})
	c.reconcileAgentSessions(t.Context(), []model.AgentSession{running, testSession("s2")}, []model.Node{sessionNode("mac-1", true)}, nil, scheduler.BuildNodeLoad(nil), trackers)
	if st := fake.status["s2"]; st["phase"] != model.AgentSessionPending || !strings.Contains(st["message"].(string), "maxMachines") {
		t.Fatalf("status = %v, want quota blocked", st)
	}
}

func TestReserveAgentSessionsSkipsEnded(t *testing.T) {
	active, ended := testSession("a"), testSession("b")
	active.Spec.NodeName, ended.Spec.NodeName = "n", "n"
	ended.Status.Phase = model.AgentSessionEnded
	load := scheduler.BuildNodeLoad(nil)
	reserveAgentSessions(load, []model.AgentSession{active, ended})
	if load["n"].Count != 1 {
		t.Fatalf("load = %+v, want only the active session", load["n"])
	}
}

func TestReconcileAgentSessionsDropsUnscheduledFinalizer(t *testing.T) {
	c, fake := newSessionTestController(t)
	s := testSession("gone")
	now := time.Now()
	s.Metadata.DeletionTimestamp = &now
	s.Metadata.Finalizers = []string{model.FinalizerAgentSession, "other"}
	c.reconcileAgentSessions(t.Context(), []model.AgentSession{s}, nil, nil, scheduler.BuildNodeLoad(nil), nil)
	meta, _ := fake.patches["gone"]["metadata"].(map[string]any)
	if fins, _ := meta["finalizers"].([]any); len(fins) != 1 || fins[0] != "other" {
		t.Fatalf("finalizers = %v", meta["finalizers"])
	}
}

func TestValidateAgentSession(t *testing.T) {
	ctl := &Controller{}
	good := testSession("ok")
	good.Spec.Policy = &model.AgentPolicy{Preset: "offline"}
	r, req := admissionReq(t, "agentsessions", "prod", admission.OperationCreate, good)
	if d := ctl.validateAgentSession(r, req); !d.Allowed {
		t.Fatalf("denied: %s", d.Reason)
	}
	bad := testSession("bad")
	bad.Spec.Workspace = &model.AgentWorkspace{Git: "ssh://git@github.com/a/b"}
	r, req = admissionReq(t, "agentsessions", "prod", admission.OperationCreate, bad)
	if d := ctl.validateAgentSession(r, req); d.Allowed || !strings.Contains(d.Reason, "spec.workspace.git") {
		t.Fatalf("decision = %+v, want a workspace.git denial", d)
	}
	r, req = admissionReq(t, "agentsessions", "prod", admission.OperationUpdate, bad)
	if d := ctl.validateAgentSession(r, req); !d.Allowed {
		t.Fatal("updates are the CRD's transition rules, not this webhook")
	}
}
