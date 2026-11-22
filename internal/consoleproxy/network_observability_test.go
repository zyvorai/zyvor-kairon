// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package consoleproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestHandleNetworkObservabilityRoutesRejectMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/network-effective/vm-1",
		"/network-stats/vm-1",
		"/network-flows/vm-1",
		"/network-drop-reasons/vm-1",
	} {
		r, _ := http.NewRequest(http.MethodGet, srv.URL+path, strings.NewReader(""))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", path, resp.StatusCode)
		}
	}
}

func TestHandleNetworkObservabilityRoutesRelayRequests(t *testing.T) {
	var gotPaths []string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/network-effective/vm-1",
		"/network-stats/vm-1",
		"/network-flows/vm-1?limit=10",
		"/network-drop-reasons/vm-1?limit=5",
	} {
		r, _ := http.NewRequest(http.MethodGet, srv.URL+path, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	expected := []string{
		"/v1/vms/vm-1/network/effective",
		"/v1/vms/vm-1/network/stats",
		"/v1/vms/vm-1/network/flows?limit=10",
		"/v1/vms/vm-1/network/drop-reasons?limit=5",
	}
	if len(gotPaths) != len(expected) {
		t.Fatalf("unexpected upstream calls: %v", gotPaths)
	}
	for i, p := range expected {
		if gotPaths[i] != p {
			t.Fatalf("call %d: expected %q, got %q", i, p, gotPaths[i])
		}
	}
}
