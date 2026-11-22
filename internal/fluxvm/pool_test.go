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

func TestCreatePool(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(PoolRecord{Name: "warm-agents", Size: 3, Members: []string{}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.CreatePool(context.Background(), PoolSpec{Name: "warm-agents", Size: 3, Template: CreateRequest{Image: "alpine-agent", Backend: "flux-vm", VCPUs: 1, MemoryMiB: 512}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/pools" || gotBody["name"] != "warm-agents" || gotBody["size"] != float64(3) {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if out.Name != "warm-agents" || out.Size != 3 {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestCreatePoolPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pool 'warm-agents' already exists", http.StatusConflict)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.CreatePool(context.Background(), PoolSpec{Name: "warm-agents", Size: 1}); err == nil {
		t.Fatal("expected an error when the pool already exists")
	}
}

func TestListPools(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pools" || r.Method != http.MethodGet {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []PoolRecord{{Name: "warm-agents", Size: 3, Members: []string{"vm-1", "vm-2"}}}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	items, err := c.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "warm-agents" || len(items[0].Members) != 2 {
		t.Fatalf("unexpected result: %+v", items)
	}
}

func TestGetPool(t *testing.T) {
	var gotPath string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(PoolRecord{Name: "warm-agents", Size: 3})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.GetPool(context.Background(), "warm-agents")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/pools/warm-agents" || out.Name != "warm-agents" {
		t.Fatalf("unexpected request/result: path=%q out=%+v", gotPath, out)
	}
}

func TestGetPoolPropagatesNotFound(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pool not found", http.StatusNotFound)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.GetPool(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown pool")
	}
}

func TestDeletePool(t *testing.T) {
	var gotPath, gotMethod string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.DeletePool(context.Background(), "warm-agents"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/pools/warm-agents" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestClaimFromPool(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(Record{UUID: "vm-1", Status: "Running"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	ttl := int64(600)
	rec, err := c.ClaimFromPool(context.Background(), "warm-agents", ClaimOverrides{Name: "agent-run", TTLSeconds: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/pools/warm-agents/claim" || gotBody["name"] != "agent-run" || gotBody["ttl_seconds"] != float64(600) {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if rec.Status != "Running" {
		t.Fatalf("unexpected result: %+v", rec)
	}
}

func TestClaimFromPoolPropagatesNoReadyMembers(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pool 'warm-agents' has no ready members right now", http.StatusConflict)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.ClaimFromPool(context.Background(), "warm-agents", ClaimOverrides{}); err == nil {
		t.Fatal("expected an error when the pool has no ready members")
	}
}
