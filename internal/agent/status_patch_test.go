// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

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

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestReconcileSkipsStatusPatchWhenUnchanged(t *testing.T) {
	machine := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod", Generation: 2},
		Spec: model.MachineSpec{
			NodeName:   "worker-1",
			Image:      model.ImageSpec{Path: "/images/db.qcow2"},
			Resources:  model.ResourceSpec{CPU: "2", Memory: "2Gi"},
			Runtime:    model.RuntimeSpec{Backend: "qemu"},
			Network:    model.NetworkSpec{Mode: "tap", NetNS: true},
			PowerState: "Running",
		},
		Status: model.MachineStatus{
			Phase:     "Running",
			NodeName:  "worker-1",
			RuntimeID: "vm-123",
			GuestIP:   "10.44.0.8",
		},
	}
	var mu sync.Mutex
	var statusPatches int
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines":
			mu.Lock()
			_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{machine}})
			mu.Unlock()
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinemigrations":
			_ = json.NewEncoder(w).Encode(model.MachineMigrationList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/machinenetworkpolicies":
			_ = json.NewEncoder(w).Encode(model.MachineNetworkPolicyList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/kairon.zyvor.dev/v1/networksecuritygroups":
			_ = json.NewEncoder(w).Encode(model.NetworkSecurityGroupList{})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
			var p struct {
				Status model.MachineStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			machine.Status = p.Status
			statusPatches++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer ks.Close()

	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-123":
			_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-123", Name: "kairon-prod-db", Status: "Running", GuestIP: "10.44.0.8"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-123/stats":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cpu_usage_percent": 55.5, "memory_usage_bytes": 999,
				"disk_read_bytes": 1, "disk_write_bytes": 2,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms":
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{{UUID: "vm-123", Name: "kairon-prod-db", Status: "Running", GuestIP: "10.44.0.8"}})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer fs.Close()

	kc, _ := kube.New(ks.URL, "", "", false)
	kc.HTTP = ks.Client()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	a := &Agent{NodeName: "worker-1", Kube: kc, Flux: fc, DefaultBackend: "qemu", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	first := statusPatches
	ltt := machine.Status.Conditions[0].LastTransitionTime
	mu.Unlock()
	if first != 1 {
		t.Fatalf("first reconcile: want 1 status patch, got %d", first)
	}

	if err := a.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	second := statusPatches
	ltt2 := machine.Status.Conditions[0].LastTransitionTime
	usage := machine.Status.ResourceUsage
	mu.Unlock()
	if second != 1 {
		t.Fatalf("steady Running tick must not patch status again, patches=%d", second)
	}
	if !ltt2.Equal(ltt) {
		t.Fatalf("LastTransitionTime reset on no-op tick: %v -> %v", ltt, ltt2)
	}
	// ResourceUsage from the first meaningful write is retained; volatile
	// stats changes on the second tick must not force another patch.
	if usage == nil {
		t.Fatal("expected ResourceUsage retained from first meaningful write")
	}
}

func TestStatusMergePatchNullsClearedFields(t *testing.T) {
	old := model.MachineStatus{
		Phase:       "Halted",
		RuntimeID:   "vm-123",
		GuestIP:     "10.44.0.8",
		Message:     "halted",
		Hibernation: &model.HibernationStatus{Node: "worker-1"},
		Lifecycle:   &model.LifecycleStatus{LastActivityTime: "2026-10-14T06:40:26Z"},
	}
	updated := old
	updated.Phase = "Stopped"
	updated.RuntimeID = ""
	updated.GuestIP = ""
	updated.Message = ""
	updated.Hibernation = nil
	updated.Lifecycle = &model.LifecycleStatus{}

	fields, err := statusMergePatch(old, updated)
	if err != nil {
		t.Fatal(err)
	}
	if fields["phase"] != "Stopped" {
		t.Fatalf("phase = %v", fields["phase"])
	}
	for _, k := range []string{"runtimeID", "guestIP", "hibernation"} {
		v, ok := fields[k]
		if !ok || v != nil {
			t.Fatalf("%s: want explicit null, got %v (present=%v)", k, v, ok)
		}
	}
	lc, _ := fields["lifecycle"].(map[string]any)
	if v, ok := lc["lastActivityTime"]; !ok || v != nil {
		t.Fatalf("nested lifecycle.lastActivityTime: want null, got %v (present=%v)", v, ok)
	}
	if _, ok := fields["nodeName"]; ok {
		t.Fatal("a field absent from both sides must stay out of the patch")
	}
}
