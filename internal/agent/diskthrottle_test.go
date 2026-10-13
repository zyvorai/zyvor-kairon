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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

type throttleCall struct {
	disk string
	body map[string]any
}

func throttleAgent(t *testing.T) (*Agent, *[]throttleCall) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []throttleCall
	)
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/throttle") {
			t.Errorf("unexpected FluxVM call %s %s", r.Method, r.URL.Path)
		}
		parts := strings.Split(r.URL.Path, "/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, throttleCall{disk: parts[len(parts)-2], body: body})
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(fs.Close)
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	return &Agent{Flux: fc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, &calls
}

func throttledMachine() model.Machine {
	return model.Machine{Spec: model.MachineSpec{
		Image: model.ImageSpec{IO: &model.DiskIO{Throttle: &model.DiskThrottle{IOPSTotal: 500}}},
		Disks: []model.MachineDisk{
			{Name: "data", ClaimName: "c", IO: &model.DiskIO{Throttle: &model.DiskThrottle{BPSTotal: 10 << 20}}},
			{Name: "pending", ClaimName: "p", IO: &model.DiskIO{Throttle: &model.DiskThrottle{IOPSRead: 10}}},
		},
	}}
}

var throttleVM = &fluxvm.Record{IDValue: "vm-1", Status: "running", Backend: "qemu"}

func TestReconcileDiskThrottlesSetsAttachedDisksAndRoot(t *testing.T) {
	a, calls := throttleAgent(t)
	got, err := a.reconcileDiskThrottles(context.Background(), throttledMachine(), throttleVM, []string{"data"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.AppliedDiskThrottle{
		{Disk: "data", Throttle: model.DiskThrottle{BPSTotal: 10 << 20}},
		{Disk: "root", Throttle: model.DiskThrottle{IOPSTotal: 500}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applied = %+v, want %+v", got, want)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %+v, want root and data only (pending is not attached)", *calls)
	}
	for _, c := range *calls {
		th, _ := c.body["throttle"].(map[string]any)
		switch c.disk {
		case "root":
			if th["iops_total"] != float64(500) {
				t.Fatalf("root body = %v", c.body)
			}
		case "data":
			if th["bps_total"] != float64(10<<20) {
				t.Fatalf("data body = %v", c.body)
			}
		default:
			t.Fatalf("unexpected disk %q", c.disk)
		}
	}
}

func TestReconcileDiskThrottlesSkipsUnchangedUnlessForced(t *testing.T) {
	a, calls := throttleAgent(t)
	m := throttledMachine()
	m.Status.AppliedDiskThrottles = []model.AppliedDiskThrottle{
		{Disk: "data", Throttle: model.DiskThrottle{BPSTotal: 10 << 20}},
		{Disk: "root", Throttle: model.DiskThrottle{IOPSTotal: 500}},
	}
	if _, err := a.reconcileDiskThrottles(context.Background(), m, throttleVM, []string{"data"}, false); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v, want none", *calls)
	}
	if _, err := a.reconcileDiskThrottles(context.Background(), m, throttleVM, []string{"data"}, true); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("forced calls = %+v, want 2", *calls)
	}
}

func TestReconcileDiskThrottlesLiftsRemovedLimits(t *testing.T) {
	a, calls := throttleAgent(t)
	m := model.Machine{Spec: model.MachineSpec{Disks: []model.MachineDisk{{Name: "data", ClaimName: "c"}}}}
	m.Status.AppliedDiskThrottles = []model.AppliedDiskThrottle{
		{Disk: "data", Throttle: model.DiskThrottle{BPSTotal: 1}},
		{Disk: "root", Throttle: model.DiskThrottle{IOPSTotal: 5}},
		{Disk: "gone", Throttle: model.DiskThrottle{IOPSTotal: 5}},
	}
	got, err := a.reconcileDiskThrottles(context.Background(), m, throttleVM, []string{"data"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("applied = %+v, want nil", got)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %+v, want data and root lifted (gone is detached)", *calls)
	}
	for _, c := range *calls {
		if v, ok := c.body["throttle"]; !ok || v != nil {
			t.Fatalf("lift body = %v, want throttle null", c.body)
		}
	}
}

func TestReconcileDiskThrottlesNoopWithoutLimits(t *testing.T) {
	a, calls := throttleAgent(t)
	got, err := a.reconcileDiskThrottles(context.Background(), model.Machine{}, throttleVM, nil, true)
	if err != nil || got != nil || len(*calls) != 0 {
		t.Fatalf("got %+v, %v, calls %+v", got, err, *calls)
	}
}

func TestReconcileDiskThrottlesRefusesOtherBackends(t *testing.T) {
	a, calls := throttleAgent(t)
	rec := &fluxvm.Record{IDValue: "vm-1", Status: "running", Backend: "cloud-hypervisor"}
	if _, err := a.reconcileDiskThrottles(context.Background(), throttledMachine(), rec, []string{"data"}, false); err == nil {
		t.Fatal("expected an error on a non-qemu VM")
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
}
