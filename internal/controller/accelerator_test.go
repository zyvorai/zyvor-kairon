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

// TestReconcileExpandsAcceleratorProfiles: a Machine naming a profile gets
// the profile's devices and the record annotation, and is scheduled with
// them in the same tick; a Machine naming a missing or live-migration
// profile stays Pending; the profiles get Ready, capacity and machine
// counts.
func TestReconcileExpandsAcceleratorProfiles(t *testing.T) {
	withAccel := func(name, profile string) model.Machine {
		return model.Machine{
			Metadata: model.ObjectMeta{Name: name, Namespace: "prod"},
			Spec: model.MachineSpec{
				PowerState:   "Running",
				Resources:    model.ResourceSpec{CPU: "1", Memory: "1Gi"},
				Accelerators: []model.MachineAccelerator{{Name: "gpu", Profile: profile}},
			},
		}
	}
	running := withAccel("old", "pair")
	running.Metadata.Annotations = map[string]string{model.AnnotationAccelerators: "gpu=pair@1"}
	running.Spec.NodeName = "worker-1"
	running.Spec.Devices = model.MachineAcceleratorProfileSpec{Profile: "mbochs-large", Count: 2}.Devices("gpu")
	running.Spec.TopologyAllocation = &model.TopologyAllocation{Devices: []model.AllocatedDevice{
		{Name: "gpu", Parent: "mbochs", Profile: "mbochs-large", UUID: "u1", FramebufferMiB: 64},
		{Name: "gpu", Parent: "mbochs", Profile: "mbochs-large", UUID: "u2", FramebufferMiB: 64},
	}}

	pair := model.MachineAcceleratorProfile{
		Metadata: model.ObjectMeta{Name: "pair", Generation: 3},
		Spec: model.MachineAcceleratorProfileSpec{
			Profile: "mbochs-large", Count: 2,
			Topology: &model.AcceleratorTopology{P2PRequired: true, SameNUMA: true},
		},
	}
	live := model.MachineAcceleratorProfile{
		Metadata: model.ObjectMeta{Name: "live", Generation: 1},
		Spec:     model.MachineAcceleratorProfileSpec{Migration: &model.AcceleratorMigration{Required: true}},
	}
	nt := model.NodeTopology{Metadata: model.ObjectMeta{Name: "worker-1"}}
	nt.Status.NUMANodes = []model.TopologyNUMANode{{ID: 0, CPUs: "0-7", MemoryMiB: 8192}}
	nt.Status.MediatedDevices = []model.TopologyMediatedParent{{
		Parent: "mbochs", Class: "gpu", Allocatable: true, NUMANode: intPtr(0), Instances: []string{"u1", "u2"},
		Types: []model.TopologyMediatedType{{ID: "mbochs-large", Available: 2, FramebufferMiB: 64}},
	}}

	var mu sync.Mutex
	patches := map[string][]map[string]any{}
	statuses := map[string]model.MachineStatus{}
	profileStatus := map[string]model.MachineAcceleratorProfileStatus{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		const machines = "/apis/kairon.zyvor.dev/v1/namespaces/prod/machines/"
		const profiles = "/apis/kairon.zyvor.dev/v1/machineacceleratorprofiles"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{
				running, withAccel("new", "pair"), withAccel("missing", "nope"), withAccel("migrating", "live"),
			}})
		case r.Method == http.MethodGet && r.URL.Path == profiles:
			_ = json.NewEncoder(w).Encode(model.MachineAcceleratorProfileList{Items: []model.MachineAcceleratorProfile{pair, live}})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, profiles+"/"):
			var p []struct {
				Value model.MachineAcceleratorProfileStatus `json:"value"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			profileStatus[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, profiles+"/"), "/status")] = p[0].Value
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/nodetopologies":
			_ = json.NewEncoder(w).Encode(model.NodeTopologyList{Items: []model.NodeTopology{nt}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			var n model.Node
			n.Metadata.Name = "worker-1"
			n.Metadata.Labels = map[string]string{model.CapableLabel: "true"}
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
			name := strings.TrimPrefix(r.URL.Path, machines)
			patches[name] = append(patches[name], body)
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

	got := patches["new"]
	if len(got) != 2 {
		t.Fatalf("want an expansion patch and a scheduling patch for new, got %+v", got)
	}
	ann := got[0]["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[model.AnnotationAccelerators] != "gpu=pair@3" {
		t.Fatalf("record annotation %+v", ann)
	}
	devs := got[0]["spec"].(map[string]any)["devices"].([]any)
	if len(devs) != 1 || devs[0].(map[string]any)["profile"] != "mbochs-large" || devs[0].(map[string]any)["count"].(float64) != 2 {
		t.Fatalf("expanded devices %+v", devs)
	}
	sched := got[1]["spec"].(map[string]any)
	alloc, _ := sched["topologyAllocation"].(map[string]any)
	if sched["nodeName"] != "worker-1" || len(alloc["devices"].([]any)) != 2 {
		t.Fatalf("scheduling patch %+v", sched)
	}

	for name, want := range map[string]string{"missing": `waiting for MachineAcceleratorProfile "nope"`, "migrating": "cannot live-migrate"} {
		if len(patches[name]) != 0 {
			t.Fatalf("%s must not be patched: %+v", name, patches[name])
		}
		st := statuses[name]
		c, _ := model.FindCondition(st.Conditions, "Scheduled")
		if st.Phase != "Pending" || c.Reason != model.ReasonAcceleratorProfileUnavailable || !strings.Contains(st.Message, want) {
			t.Fatalf("%s status %+v", name, st)
		}
	}

	ps := profileStatus["pair"]
	ready, _ := model.FindCondition(ps.Conditions, model.AcceleratorProfileReady)
	avail, _ := model.FindCondition(ps.Conditions, model.AcceleratorProfileAvailable)
	if ready.Status != "True" || ps.Machines != 2 || ps.ObservedGeneration != 3 {
		t.Fatalf("pair status %+v", ps)
	}
	// Two instances left before this tick placed "new".
	if ps.Capacity != 1 || len(ps.Nodes) != 1 || ps.Nodes[0].Fits != 1 || avail.Status != "True" {
		t.Fatalf("pair capacity %+v", ps)
	}
	ls := profileStatus["live"]
	ready, _ = model.FindCondition(ls.Conditions, model.AcceleratorProfileReady)
	if ready.Status != "False" || ready.Reason != model.AcceleratorProfileReasonLiveMigrate || ls.Nodes != nil {
		t.Fatalf("live status %+v", ls)
	}
}

func TestExpandAcceleratorsIsIdempotent(t *testing.T) {
	p := model.MachineAcceleratorProfile{
		Metadata: model.ObjectMeta{Name: "nic-gpu", Generation: 2},
		Spec:     model.MachineAcceleratorProfileSpec{Vendor: "nvidia", NIC: &model.AcceleratorNIC{Vendor: "mellanox"}},
	}
	m := model.Machine{Spec: model.MachineSpec{Accelerators: []model.MachineAccelerator{{Name: "train", Profile: "nic-gpu"}}}}
	byName := map[string]model.MachineAcceleratorProfile{"nic-gpu": p}
	devs, record, reason := expandAccelerators(m, byName)
	if reason != "" || len(devs) != 2 || devs[1].Name != "train-nic" || record != "train=nic-gpu@2" {
		t.Fatalf("devs %+v record %q reason %q", devs, record, reason)
	}
	m.Spec.Devices = devs
	again, _, reason := expandAccelerators(m, byName)
	if reason != "" || len(again) != 2 {
		t.Fatalf("second expansion %+v %q", again, reason)
	}

	m.Spec.Devices = make([]model.MachineDevice, 7)
	for i := range m.Spec.Devices {
		m.Spec.Devices[i] = model.MachineDevice{Name: string(rune('a' + i)), Class: "nvme"}
	}
	if _, _, reason := expandAccelerators(m, byName); !strings.Contains(reason, "at most 8") {
		t.Fatalf("over the device limit: %q", reason)
	}
}
