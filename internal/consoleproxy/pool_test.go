// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package consoleproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestHandlePoolRoutesRejectMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, "/pools", `{}`},
		{http.MethodGet, "/pools", ""},
		{http.MethodGet, "/pools/x", ""},
		{http.MethodDelete, "/pools/x", ""},
		{http.MethodPost, "/pools/x/claim", `{}`},
	} {
		r, _ := http.NewRequest(req.method, srv.URL+req.path, strings.NewReader(req.body))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401, got %d", req.method, req.path, resp.StatusCode)
		}
	}
}

func TestHandleCreatePoolRelaysRequest(t *testing.T) {
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pools" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.PoolRecord{Name: "warm-agents", Size: 3})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"name":"warm-agents","size":3,"template":{"name":"","backend":"flux-vm","image":"alpine-agent","vcpus":1,"memory_mib":512}}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/pools", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotBody["name"] != "warm-agents" || gotBody["size"] != float64(3) {
		t.Fatalf("unexpected upstream request: %+v", gotBody)
	}
}

func TestHandleListPoolsRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pools" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []fluxvm.PoolRecord{{Name: "warm-agents"}}})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pools", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestHandleDeletePoolRelaysRequest(t *testing.T) {
	var gotPath, gotMethod string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/pools/warm-agents", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/pools/warm-agents" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected upstream request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestHandleClaimPoolRelaysRequestAndResponse(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.Record{UUID: "vm-1", Status: "Running"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"name":"agent-run"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/pools/warm-agents/claim", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/pools/warm-agents/claim" || gotBody["name"] != "agent-run" {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
	var out fluxvm.Record
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "Running" {
		t.Fatalf("unexpected response: %+v", out)
	}
}
