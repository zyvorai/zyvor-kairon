// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

type countingDest struct {
	prepareN, commitN, abortN int
}

func (c *countingDest) Prepare(context.Context, Session) (PrepareResult, error) {
	c.prepareN++
	return PrepareResult{TransferSupported: true, Endpoint: "unix:///tmp/x", Backend: "stub"}, nil
}
func (c *countingDest) Commit(context.Context, Session) error { c.commitN++; return nil }
func (c *countingDest) Abort(context.Context, Session) error  { c.abortN++; return nil }

func TestNetworkAwareDestinationRestoreResume(t *testing.T) {
	var restore, resume bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/rt-1/network/migration/restore":
			restore = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms/rt-1/network/migration/resume":
			resume = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected", 404)
		}
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	inner := &countingDest{}
	d := NetworkAwareDestination{Inner: inner, Flux: fc}
	session := Session{ID: "s1", Namespace: "ns", Machine: "m", SourceNode: "a", TargetNode: "b", RuntimeID: "rt-1", NetworkSnapshot: json.RawMessage(`{"v":1}`)}
	if _, err := d.Prepare(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if !restore || !resume || inner.prepareN != 1 || inner.commitN != 1 {
		t.Fatalf("restore=%v resume=%v inner=%+v", restore, resume, inner)
	}
}

func TestNetworkAwareDestinationDiagnoseFindsDestinationRuntime(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/vms" && r.URL.Query().Get("name") == "kairon-ns-m" {
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{{UUID: "rt-2", Name: "kairon-ns-m", Status: "Running", GuestIP: "10.0.0.5"}})
			return
		}
		http.Error(w, "unexpected "+r.URL.String(), 404)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	d := NetworkAwareDestination{Inner: &countingDest{}, Flux: fc}
	session := Session{ID: "s1", Namespace: "ns", Machine: "m", SourceNode: "a", TargetNode: "b", Phase: "Prepared"}
	result, err := d.Diagnose(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionPhase != "Prepared" || !result.DestinationRuntimeFound || result.DestinationRuntimeStatus != "Running" || result.DestinationRuntimeID != "rt-2" || result.DestinationGuestIP != "10.0.0.5" {
		t.Fatalf("unexpected diagnosis: %+v", result)
	}
}

func TestNetworkAwareDestinationDiagnoseReportsNotFound(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/vms" {
			_ = json.NewEncoder(w).Encode([]fluxvm.Record{})
			return
		}
		http.Error(w, "unexpected "+r.URL.String(), 404)
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	d := NetworkAwareDestination{Inner: &countingDest{}, Flux: fc}
	session := Session{ID: "s1", Namespace: "ns", Machine: "m", SourceNode: "a", TargetNode: "b", Phase: "Prepared"}
	result, err := d.Diagnose(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionPhase != "Prepared" || result.DestinationRuntimeFound {
		t.Fatalf("expected DestinationRuntimeFound=false, got %+v", result)
	}
}

type adoptingDest struct{ countingDest }

func (a *adoptingDest) Prepare(context.Context, Session) (PrepareResult, error) {
	a.prepareN++
	return PrepareResult{TransferSupported: true, Endpoint: "tcp:10.0.0.9:1", Backend: "qemu", DestinationRuntimeID: "recv-1"}, nil
}

func TestNetworkAwareDestinationRestoresOnTheDestinationRuntimeAfterCommit(t *testing.T) {
	var calls []string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fs.Close()
	fc := fluxvm.New(fs.URL, "")
	fc.HTTP = fs.Client()
	inner := &adoptingDest{}
	d := NetworkAwareDestination{Inner: inner, Flux: fc}
	session := Session{ID: "s1", Namespace: "ns", Machine: "m", SourceNode: "a", TargetNode: "b", RuntimeID: "src-1", NetworkSnapshot: json.RawMessage(`{"v":1}`)}
	res, err := d.Prepare(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("network touched before commit: %v", calls)
	}
	session.DestinationRuntimeID = res.DestinationRuntimeID
	if err := d.Commit(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/vms/recv-1/network/migration/restore", "/v1/vms/recv-1/network/migration/resume"}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if inner.commitN != 1 {
		t.Fatalf("inner commit = %d", inner.commitN)
	}
}
