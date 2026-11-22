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

func TestHandleDiagnosticsRoutesRejectMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/capabilities"},
		{http.MethodGet, "/pressure/vm-1"},
		{http.MethodGet, "/cpuset/vm-1"},
		{http.MethodPost, "/freeze/vm-1"},
		{http.MethodPost, "/thaw/vm-1"},
		{http.MethodGet, "/frozen/vm-1"},
	} {
		r, _ := http.NewRequest(req.method, srv.URL+req.path, strings.NewReader(""))
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

func TestHandleRuntimeCapabilitiesRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/capabilities" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(fluxvm.RuntimeCapabilities{APIVersion: "runtime.fluxvm.zyvor.io/v1"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/capabilities", nil)
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

func TestHandlePressureRelaysResponse(t *testing.T) {
	var gotPath string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(fluxvm.Pressure{CPUSome: &fluxvm.PressureRecord{Avg10: 1.2}})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/pressure/vm-1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/vms/vm-1/pressure" {
		t.Fatalf("unexpected upstream path: %q", gotPath)
	}
}

func TestHandleFreezeThawFrozenRelayRequests(t *testing.T) {
	var calls []string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/vms/vm-1/freeze", "/v1/vms/vm-1/thaw":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/vms/vm-1/frozen":
			_ = json.NewEncoder(w).Encode(map[string]any{"frozen": true})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, req := range []struct{ method, path string }{
		{http.MethodPost, "/freeze/vm-1"},
		{http.MethodGet, "/frozen/vm-1"},
		{http.MethodPost, "/thaw/vm-1"},
	} {
		r, _ := http.NewRequest(req.method, srv.URL+req.path, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d", req.method, req.path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if len(calls) != 3 || calls[0] != "POST /v1/vms/vm-1/freeze" || calls[1] != "GET /v1/vms/vm-1/frozen" || calls[2] != "POST /v1/vms/vm-1/thaw" {
		t.Fatalf("unexpected upstream calls: %v", calls)
	}
}
