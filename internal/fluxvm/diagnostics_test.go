// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetRuntimeCapabilities(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/capabilities" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(RuntimeCapabilities{
			APIVersion: "runtime.fluxvm.zyvor.io/v1", Scope: "node-local", OrchestrationOwner: "zyvor-fabric",
			Migration: []RuntimeMigrationCapability{{Backend: "qemu", Live: true, PreCopy: true, PostCopy: true, Multifd: true, RequiresSharedStorage: true, Transports: []string{"tcp"}}},
			Snapshot:  []RuntimeSnapshotCapability{{Backend: "qemu", Memory: true, Disk: true, Portable: false}},
		})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.GetRuntimeCapabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if out.APIVersion != "runtime.fluxvm.zyvor.io/v1" || len(out.Migration) != 1 || out.Migration[0].Backend != "qemu" || !out.Migration[0].Live {
		t.Fatalf("unexpected result: %+v", out)
	}
	if len(out.Snapshot) != 1 || !out.Snapshot[0].Memory {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestGetPressure(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(Pressure{CPUSome: &PressureRecord{Avg10: 1.5, Avg60: 0.8, Avg300: 0.2, Total: 1000}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.GetPressure(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/pressure" || out.CPUSome == nil || out.CPUSome.Avg10 != 1.5 {
		t.Fatalf("unexpected request/result: path=%q out=%+v", gotPath, out)
	}
}

func TestGetCPUSet(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"cpus": []uint32{2, 3, 4, 5}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	cpus, err := c.GetCPUSet(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/cpuset" || len(cpus) != 4 || cpus[0] != 2 {
		t.Fatalf("unexpected request/result: path=%q cpus=%v", gotPath, cpus)
	}
}

func TestFreezeThawIsFrozen(t *testing.T) {
	frozen := false
	var calls []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/vm-1/freeze":
			frozen = true
			w.WriteHeader(http.StatusNoContent)
		case "/v1/vms/vm-1/thaw":
			frozen = false
			w.WriteHeader(http.StatusNoContent)
		case "/v1/vms/vm-1/frozen":
			_ = json.NewEncoder(w).Encode(map[string]any{"frozen": frozen})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if got, err := c.IsFrozen(context.Background(), "vm-1"); err != nil || got {
		t.Fatalf("expected not frozen initially, got %v err=%v", got, err)
	}
	if err := c.Freeze(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.IsFrozen(context.Background(), "vm-1"); err != nil || !got {
		t.Fatalf("expected frozen after Freeze, got %v err=%v", got, err)
	}
	if err := c.Thaw(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.IsFrozen(context.Background(), "vm-1"); err != nil || got {
		t.Fatalf("expected not frozen after Thaw, got %v err=%v", got, err)
	}
	if len(calls) != 5 {
		t.Fatalf("unexpected call sequence: %v", calls)
	}
}

func TestFreezePropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cgroup freezer unavailable", http.StatusInternalServerError)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.Freeze(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}
