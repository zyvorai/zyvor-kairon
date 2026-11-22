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

func TestSnapshot(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tag": "before-upgrade"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.Snapshot(context.Background(), "vm-1", "before-upgrade"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/snapshot" || gotBody["tag"] != "before-upgrade" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestSnapshotPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "snapshot not supported for backend Firecracker", http.StatusBadRequest)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if err := c.Snapshot(context.Background(), "vm-1", "tag"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestRestoreSnapshotOrchestratesStopThenStartFromSnapshot(t *testing.T) {
	var calls []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/vm-1/stop":
			_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Stopped"})
		case "/v1/vms/vm-1/start-from-snapshot":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["tag"] != "before-upgrade" {
				t.Fatalf("expected the tag to be forwarded, got %+v", body)
			}
			_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Running"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	rec, err := c.RestoreSnapshot(context.Background(), "vm-1", "before-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "Running" {
		t.Fatalf("unexpected result: %+v", rec)
	}
	if len(calls) != 2 || calls[0] != "POST /v1/vms/vm-1/stop" || calls[1] != "POST /v1/vms/vm-1/start-from-snapshot" {
		t.Fatalf("expected stop then start-from-snapshot in order, got %v", calls)
	}
}

func TestRestoreSnapshotStopsBeforeReportingStartFailure(t *testing.T) {
	var stopCalled bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/stop":
			stopCalled = true
			_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Stopped"})
		case "/v1/vms/vm-1/start-from-snapshot":
			http.Error(w, "no such snapshot", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.RestoreSnapshot(context.Background(), "vm-1", "missing-tag"); err == nil {
		t.Fatal("expected an error when start-from-snapshot fails")
	}
	if !stopCalled {
		t.Fatal("expected stop to have been called before the failing start-from-snapshot")
	}
}

func TestRestoreSnapshotNeverCallsStartFromSnapshotWhenStopFails(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vms/vm-1/stop":
			http.Error(w, "VM not found", http.StatusNotFound)
		case "/v1/vms/vm-1/start-from-snapshot":
			t.Fatal("start-from-snapshot should never be called when stop itself fails")
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.RestoreSnapshot(context.Background(), "vm-1", "tag"); err == nil {
		t.Fatal("expected an error when stop fails")
	}
}

func TestStop(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Stopped"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	rec, err := c.Stop(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/stop" || rec.Status != "Stopped" {
		t.Fatalf("unexpected result: path=%q rec=%+v", gotPath, rec)
	}
}

func TestStopPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "VM not found", http.StatusNotFound)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.Stop(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}

func TestStart(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	rec, err := c.Start(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/start" || rec.Status != "Running" {
		t.Fatalf("unexpected result: path=%q rec=%+v", gotPath, rec)
	}
}

func TestStartPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "disk no longer exists", http.StatusInternalServerError)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.Start(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}
