// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestFluxExtRoutesRelayToFixedUpstreamPaths(t *testing.T) {
	var gotMethod, gotURI, gotBody string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotURI, gotBody = r.Method, r.URL.RequestURI(), string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cases := []struct {
		method, path, body, wantMethod, wantURI, wantBody string
	}{
		{"POST", "/fork-plan/vm-1", `{"count":3}`, "POST", "/v1/vms/vm-1/fork/plan", `{"count":3}`},
		{"GET", "/host-memory", "", "GET", "/v1/host/memory", ""},
		{"GET", "/events?limit=5&vm=vm-1", "", "GET", "/v1/events?limit=5&vm=vm-1", ""},
		{"GET", "/branches", "", "GET", "/v1/branches", ""},
		{"GET", "/branches/a/diff/b", "", "GET", "/v1/branches/a/diff/b", ""},
		{"GET", "/host-topology", "", "GET", "/v1/host/topology", ""},
		{"GET", "/host-pressure", "", "GET", "/v1/host/pressure", ""},
		{"POST", "/branches/b1/start", `{"count":2}`, "POST", "/v1/branches/b1/start", `{"count":2}`},
		{"DELETE", "/branches/b1", "", "DELETE", "/v1/branches/b1", ""},
		{"POST", "/branches/b1/start-plan", `{"count":1}`, "POST", "/v1/branches/b1/start/plan", `{"count":1}`},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d", c.method, c.path, resp.StatusCode)
		}
		if gotMethod != c.wantMethod || gotURI != c.wantURI || gotBody != c.wantBody {
			t.Fatalf("%s %s: upstream got %s %s %q, want %s %s %q", c.method, c.path,
				gotMethod, gotURI, gotBody, c.wantMethod, c.wantURI, c.wantBody)
		}
	}
}

func TestFluxExtRoutesRequireToken(t *testing.T) {
	s := &Server{Flux: fluxvm.New("http://127.0.0.1:1", ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, p := range []string{"GET /host-memory", "GET /events", "GET /branches", "POST /fork-plan/x"} {
		parts := strings.SplitN(p, " ", 2)
		req, _ := http.NewRequest(parts[0], srv.URL+parts[1], nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", p, resp.StatusCode)
		}
	}
}

func TestRelayFluxSurfacesUpstreamErrorsAsBadGateway(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusConflict)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/branches", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
}

func TestEgressNftablesIsWrappedInJSON(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/egress/nftables" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("table inet x {}\n"))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/egress-nftables", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"snippet":"table inet x {}\n"`) {
		t.Fatalf("unexpected reply %d %s", resp.StatusCode, b)
	}
}

func TestEventsStreamRequiresTokenAndProxiesUpstream(t *testing.T) {
	var gotAuth string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/events/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"event\":\"vm.start\"}\n\n"))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, "fluxtok"), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/events/stream", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "vm.start") || gotAuth != "Bearer fluxtok" {
		t.Fatalf("stream %q, upstream auth %q", b, gotAuth)
	}
}
