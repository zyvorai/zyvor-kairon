// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package macnode

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

var mac = Info{CPUs: 10, MemoryMiB: 16384, Arch: "arm64", OS: "darwin", InternalIP: "192.168.1.20"}

func TestSystemReserveLeavesRoomForMacOS(t *testing.T) {
	for _, c := range []struct{ total, want uint64 }{{16384, 4096}, {8192, 3072}, {65536, 16384}, {2048, 2048}} {
		if got := SystemReserveMiB(c.total); got != c.want {
			t.Errorf("SystemReserveMiB(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

func TestAllocatableSubtractsTheReserve(t *testing.T) {
	a := Allocatable(mac)
	if a["cpu"] != "10" || a["memory"] != "12288Mi" || a["pods"] != "0" {
		t.Fatalf("allocatable = %v", a)
	}
	// The scheduler parses these with model.ParseMemoryMiB / ParseVCPUs.
	if m, err := model.ParseMemoryMiB(a["memory"]); err != nil || m != 12288 {
		t.Fatalf("memory %q does not parse for the scheduler: %v %d", a["memory"], err, m)
	}
	if c, err := model.ParseVCPUs(a["cpu"]); err != nil || c != 10 {
		t.Fatalf("cpu %q does not parse for the scheduler: %v %d", a["cpu"], err, c)
	}
}

func TestLabelsMakeTheNodeSchedulable(t *testing.T) {
	l := Labels("macmini", mac)
	for k, v := range map[string]string{"kubernetes.io/arch": "arm64", "kubernetes.io/os": "darwin", model.CapableLabel: "true", LabelBackendVZ: "true"} {
		if l[k] != v {
			t.Errorf("label %s = %q, want %q", k, l[k], v)
		}
	}
	if _, ok := l[LabelMLX]; ok {
		t.Error("the MLX label must only appear when an inference endpoint is configured")
	}
	withMLX := mac
	withMLX.MLXURL = "http://127.0.0.1:8780/v1"
	if Labels("macmini", withMLX)[LabelMLX] != "true" {
		t.Error("expected the MLX label when MLXURL is set")
	}
}

func TestLinuxNodeIsNotLabelledVZ(t *testing.T) {
	linux := Info{CPUs: 12, MemoryMiB: 32768, Arch: "amd64", OS: "linux", InternalIP: "175.110.122.71"}
	l := Labels("lab-2", linux)
	if _, ok := l[LabelBackendVZ]; ok {
		t.Fatalf("a Linux host must not claim the vz backend: %v", l)
	}
	if l["kubernetes.io/os"] != "linux" || l[model.CapableLabel] != "true" {
		t.Fatalf("labels = %v", l)
	}
}

func TestBuiltNodeIsTaintedVMOnlyAndReady(t *testing.T) {
	n := BuildNode("macmini", mac)
	taints := n["spec"].(map[string]any)["taints"].([]map[string]string)
	if len(taints) != 1 || taints[0]["key"] != TaintVMOnly || taints[0]["effect"] != "NoSchedule" {
		t.Fatalf("taints = %v", taints)
	}
	st := BuildStatus(mac, time.Date(2026, 10, 9, 1, 2, 3, 0, time.UTC))["status"].(map[string]any)
	cond := st["conditions"].([]map[string]string)[0]
	if cond["type"] != "Ready" || cond["status"] != "True" || cond["lastHeartbeatTime"] != "2026-10-09T01:02:03Z" {
		t.Fatalf("condition = %v", cond)
	}
	if st["addresses"].([]map[string]string)[0]["address"] != "192.168.1.20" {
		t.Fatalf("addresses = %v", st["addresses"])
	}
}

type fakeAPI struct {
	exists  bool
	created []map[string]any
	patched []map[string]any
	status  []map[string]any
}

func (f *fakeAPI) GetNode(context.Context, string) (model.Node, error) {
	if !f.exists {
		return model.Node{}, &kube.APIError{StatusCode: http.StatusNotFound}
	}
	return model.Node{}, nil
}
func (f *fakeAPI) CreateNode(_ context.Context, n map[string]any) error {
	f.created = append(f.created, n)
	f.exists = true
	return nil
}
func (f *fakeAPI) PatchNode(_ context.Context, _ string, p map[string]any) error {
	f.patched = append(f.patched, p)
	return nil
}
func (f *fakeAPI) PatchNodeStatus(_ context.Context, _ string, p map[string]any) error {
	f.status = append(f.status, p)
	return nil
}

func TestRegisterCreatesOnceThenOnlyRefreshes(t *testing.T) {
	f := &fakeAPI{}
	now := time.Now()
	if err := Register(context.Background(), f, "macmini", mac, now); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || len(f.status) != 1 || len(f.patched) != 0 {
		t.Fatalf("first registration: created=%d status=%d patched=%d", len(f.created), len(f.status), len(f.patched))
	}
	if err := Register(context.Background(), f, "macmini", mac, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 || len(f.status) != 2 || len(f.patched) != 1 {
		t.Fatalf("heartbeat: created=%d status=%d patched=%d", len(f.created), len(f.status), len(f.patched))
	}
}

type failingAPI struct{ fakeAPI }

func (f *failingAPI) GetNode(context.Context, string) (model.Node, error) {
	return model.Node{}, &kube.APIError{StatusCode: http.StatusForbidden}
}

func TestRegisterReportsAnAuthorizationFailureInsteadOfCreating(t *testing.T) {
	f := &failingAPI{}
	if err := Register(context.Background(), f, "macmini", mac, time.Now()); err == nil {
		t.Fatal("a 403 must be returned, not treated as 'node missing'")
	}
	if len(f.created) != 0 {
		t.Fatal("must not create a node after a non-404 error")
	}
}
