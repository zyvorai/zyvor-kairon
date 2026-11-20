// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetVMNetworkEffective(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"vm_id":"vm-1","policy":{}}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	data, err := c.GetVMNetworkEffective(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/network/effective" {
		t.Fatalf("unexpected path: %q", gotPath)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out["vm_id"] != "vm-1" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestGetVMNetworkStats(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"rx_bytes":100,"tx_bytes":200}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	data, err := c.GetVMNetworkStats(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/network/stats" || len(data) == 0 {
		t.Fatalf("unexpected request/result: path=%q data=%s", gotPath, data)
	}
}

func TestGetVMNetworkFlows(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.GetVMNetworkFlows(context.Background(), "vm-1", 50); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/network/flows?limit=50" {
		t.Fatalf("unexpected request: %q", gotPath)
	}
}

func TestGetVMNetworkFlowsOmitsLimitWhenZero(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.GetVMNetworkFlows(context.Background(), "vm-1", 0); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/network/flows" {
		t.Fatalf("unexpected request: %q", gotPath)
	}
}

func TestGetVMNetworkDropReasons(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"items":[{"reason":"POLICY_DENY"}]}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	data, err := c.GetVMNetworkDropReasons(context.Background(), "vm-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/network/drop-reasons?limit=20" {
		t.Fatalf("unexpected request: %q", gotPath)
	}
	if len(data) == 0 {
		t.Fatal("expected a non-empty response")
	}
}

func TestGetVMNetworkStatsPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "vm not found", http.StatusNotFound)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.GetVMNetworkStats(context.Background(), "vm-1"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}
