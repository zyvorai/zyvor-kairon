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

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// TestReconcileSchedulesMachineWithCPUPinningAtomically proves the "which
// node" and "which cores" scheduling decisions land in a single patch --
// spec.nodeName and spec.resources.allocatedCpuSet both present in the
// exact same PATCH call, never two separate writes that could land
// separately.
func TestReconcileSchedulesMachineWithCPUPinningAtomically(t *testing.T) {
	var patchBody map[string]any
	var patchCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{
				Metadata: model.ObjectMeta{Name: "pinned", Namespace: "prod"},
				Spec:     model.MachineSpec{PowerState: "Running", Resources: model.ResourceSpec{CPU: "2", CPUPinning: true}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true", model.PinnableCPUsLabel: "2-9"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/pinned":
			patchCount++
			_ = json.NewDecoder(r.Body).Decode(&patchBody)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if patchCount != 1 {
		t.Fatalf("expected exactly one PATCH call, got %d", patchCount)
	}
	spec, _ := patchBody["spec"].(map[string]any)
	if spec == nil || spec["nodeName"] != "worker-1" {
		t.Fatalf("unexpected patch body: %+v", patchBody)
	}
	resources, _ := spec["resources"].(map[string]any)
	if resources == nil {
		t.Fatalf("expected spec.resources.allocatedCpuSet in the same patch, got %+v", spec)
	}
	cpuset, _ := resources["allocatedCpuSet"].([]any)
	if len(cpuset) != 2 || cpuset[0] != float64(2) || cpuset[1] != float64(3) {
		t.Fatalf("unexpected allocatedCpuSet: %+v", resources["allocatedCpuSet"])
	}
}

// TestReconcileLeavesCPUPinningMachinePendingWhenNoNodeHasEnoughCores
// proves a genuine capacity shortfall (here, no node asserting
// PinnableCPUsLabel at all) surfaces as a clear Pending status/message via
// Choose's own existing no-eligible-node path, not a hard reconcile error
// or a silent partial allocation -- and that no spec patch (node
// assignment or otherwise) happens when that's the outcome.
func TestReconcileLeavesCPUPinningMachinePendingWhenNoNodeHasEnoughCores(t *testing.T) {
	var gotStatus model.MachineStatus
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{
				Metadata: model.ObjectMeta{Name: "pinned", Namespace: "prod"},
				Spec:     model.MachineSpec{PowerState: "Running", Resources: model.ResourceSpec{CPU: "8", CPUPinning: true}},
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"} // no PinnableCPUsLabel at all
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/pinned/status":
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			gotStatus = p.Status
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/pinned":
			t.Fatal("expected no spec patch when no node has enough pinnable capacity")
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	ctl := &Controller{Kube: kc, Scheduler: scheduler.Scheduler{RequireCapableLabel: true}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := ctl.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotStatus.Phase != "Pending" || gotStatus.Message == "" {
		t.Fatalf("expected a Pending status with a clear message, got %+v", gotStatus)
	}
}
