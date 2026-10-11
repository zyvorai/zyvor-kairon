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

// TestReconcileSchedulesTopologyMachinesAtomically checks that nodeName,
// allocatedCpuSet and topologyAllocation land in one patch, that two
// Machines in the same tick get different NUMA nodes, and that a Machine
// no NUMA node can hold stays Pending with reason TopologyUnsatisfiable.
func TestReconcileSchedulesTopologyMachinesAtomically(t *testing.T) {
	topoMachine := func(name, cpu string) model.Machine {
		return model.Machine{
			Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
			Spec: model.MachineSpec{
				PowerState: "Running",
				Resources:  model.ResourceSpec{CPU: cpu, Memory: "2Gi", CPUPinning: true},
				Topology:   &model.TopologySpec{Policy: model.TopologyPolicySingleNUMANode},
			},
		}
	}
	nt := model.NodeTopology{Metadata: model.ObjectMeta{Name: "worker-1"}}
	nt.Status.NUMABinding = true
	nt.Status.NUMANodes = []model.TopologyNUMANode{
		{ID: 0, CPUs: "0-7", MemoryMiB: 8192},
		{ID: 1, CPUs: "8-15", MemoryMiB: 8192},
	}

	var mu sync.Mutex
	patches := map[string]map[string]any{}
	statuses := map[string]model.MachineStatus{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const machines = "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{
				topoMachine("a", "5"), topoMachine("b", "5"), topoMachine("huge", "6"),
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/nodetopologies":
			_ = json.NewEncoder(w).Encode(model.NodeTopologyList{Items: []model.NodeTopology{nt}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true", model.PinnableCPUsLabel: "0-15"}
			n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: "True"}}
			_ = json.NewEncoder(w).Encode(model.NodeList{Items: []model.Node{n}})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status") && strings.HasPrefix(r.URL.Path, machines):
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			statuses[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, machines), "/status")] = p.Status
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, machines):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			patches[strings.TrimPrefix(r.URL.Path, machines)] = body
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

	numaOf := func(name string) float64 {
		t.Helper()
		spec, _ := patches[name]["spec"].(map[string]any)
		if spec == nil || spec["nodeName"] != "worker-1" {
			t.Fatalf("%s: want nodeName in the patch, got %+v", name, patches[name])
		}
		res, _ := spec["resources"].(map[string]any)
		if cpus, _ := res["allocatedCpuSet"].([]any); len(cpus) != 5 {
			t.Fatalf("%s: want 5 CPUs in the same patch, got %+v", name, res)
		}
		alloc, _ := spec["topologyAllocation"].(map[string]any)
		nodes, _ := alloc["numaNodes"].([]any)
		if len(nodes) != 1 {
			t.Fatalf("%s: want one NUMA node in topologyAllocation, got %+v", name, alloc)
		}
		return nodes[0].(map[string]any)["id"].(float64)
	}
	if numaOf("a") == numaOf("b") {
		t.Fatal("two Machines scheduled in one tick got the same NUMA node")
	}
	if _, ok := patches["huge"]; ok {
		t.Fatal("a Machine no NUMA node can hold must not be assigned")
	}
	st := statuses["huge"]
	if st.Phase != "Pending" || len(st.Conditions) == 0 || st.Conditions[len(st.Conditions)-1].Reason != scheduler.TopologyUnsatisfiable {
		t.Fatalf("want Pending with reason %s, got %+v", scheduler.TopologyUnsatisfiable, st)
	}
}
