// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildDRAHintsSkipsAPICallsWithNoDeviceClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected call: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc}

	machines := []model.Machine{{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}}
	hints, err := c.buildDRAHints(context.Background(), machines)
	if err != nil {
		t.Fatalf("buildDRAHints: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("got %v, want no hints", hints)
	}
}

func TestBuildDRAHintsResolvesAllocatedDeviceToItsNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/resource.k8s.io/v1/resourceslices":
			_ = json.NewEncoder(w).Encode(model.ResourceSliceList{Items: []model.ResourceSlice{
				{Metadata: model.ObjectMeta{Name: "slice-1"}, Spec: model.ResourceSliceSpec{Driver: "gpu.example.com", Pool: model.ResourceSlicePoolInfo{Name: "pool-a"}, NodeName: "worker-2"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/resource.k8s.io/v1/resourceclaims":
			_ = json.NewEncoder(w).Encode(model.ResourceClaimList{Items: []model.ResourceClaim{
				{
					Metadata: model.ObjectMeta{Name: "gpu-claim", Namespace: "default"},
					Status: model.ResourceClaimStatus{Allocation: &model.ResourceClaimAllocation{
						Devices: model.ResourceClaimDeviceAllocation{Results: []model.DeviceRequestAllocationResult{
							{Driver: "gpu.example.com", Pool: "pool-a", Device: "gpu-0"},
						}},
					}},
				},
			}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc}

	machines := []model.Machine{{
		Metadata: model.ObjectMeta{Name: "gpu-vm", Namespace: "default"},
		Spec:     model.MachineSpec{DeviceClaims: []model.DeviceClaimReference{{Name: "gpu-claim"}}},
	}}
	hints, err := c.buildDRAHints(context.Background(), machines)
	if err != nil {
		t.Fatalf("buildDRAHints: %v", err)
	}
	if got := hints["default/gpu-vm"]; got != "worker-2" {
		t.Fatalf("got hint %q, want worker-2", got)
	}
}

func TestBuildDRAHintsSkipsUnallocatedClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/apis/resource.k8s.io/v1/resourceslices":
			_ = json.NewEncoder(w).Encode(model.ResourceSliceList{})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/resource.k8s.io/v1/resourceclaims":
			_ = json.NewEncoder(w).Encode(model.ResourceClaimList{Items: []model.ResourceClaim{
				{Metadata: model.ObjectMeta{Name: "gpu-claim", Namespace: "default"}}, // no status.allocation yet
			}})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc}

	machines := []model.Machine{{
		Metadata: model.ObjectMeta{Name: "gpu-vm", Namespace: "default"},
		Spec:     model.MachineSpec{DeviceClaims: []model.DeviceClaimReference{{Name: "gpu-claim"}}},
	}}
	hints, err := c.buildDRAHints(context.Background(), machines)
	if err != nil {
		t.Fatalf("buildDRAHints: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("got %v, want no hints for an unallocated claim", hints)
	}
}

func TestBuildDRAHintsSoftFailsWhenDRAAPINotInstalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()
	c := &Controller{Kube: kc}

	machines := []model.Machine{{
		Metadata: model.ObjectMeta{Name: "gpu-vm", Namespace: "default"},
		Spec:     model.MachineSpec{DeviceClaims: []model.DeviceClaimReference{{Name: "gpu-claim"}}},
	}}
	hints, err := c.buildDRAHints(context.Background(), machines)
	if err != nil {
		t.Fatalf("expected a 404 (DRA API not installed) to be treated as no hints, not an error: %v", err)
	}
	if len(hints) != 0 {
		t.Fatalf("got %v, want no hints", hints)
	}
}
