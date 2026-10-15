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

const testSessionID = "0b8f6d3e-6a39-4d0f-9a43-6f1e9f6d2c11"

func agentSessionRelay(t *testing.T) (relay *httptest.Server, seen *[]*http.Request) {
	t.Helper()
	var reqs []*http.Request
	flux := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	}))
	t.Cleanup(flux.Close)
	s := &Server{Flux: fluxvm.New(flux.URL, "flux-token"), Token: "secret"}
	relay = httptest.NewServer(s.Handler())
	t.Cleanup(relay.Close)
	return relay, &reqs
}

func relayDo(t *testing.T, method, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAgentSessionRelayForwardsAllowedVerbs(t *testing.T) {
	relay, seen := agentSessionRelay(t)
	resp := relayDo(t, http.MethodGet, relay.URL+"/agent-sessions/"+testSessionID+"/jobs/j1?offset=10&wait_ms=500", //nolint:bodyclose // helper closes the body
		map[string]string{"Cookie": "kairon=x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := (*seen)[0]
	if got.URL.Path != "/v1/agents/sessions/"+testSessionID+"/jobs/j1" || got.URL.RawQuery != "offset=10&wait_ms=500" {
		t.Fatalf("upstream = %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer flux-token" {
		t.Fatalf("upstream Authorization not swapped for the FluxVM token")
	}
	if got.Header.Get("Cookie") != "" {
		t.Fatal("cookie leaked to FluxVM")
	}

	resp = relayDo(t, http.MethodGet, relay.URL+"/agent-sessions/"+testSessionID, nil) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK || (*seen)[1].URL.Path != "/v1/agents/sessions/"+testSessionID {
		t.Fatalf("bare session GET: status %d", resp.StatusCode)
	}
}

func TestAgentSessionRelayRefuses(t *testing.T) {
	relay, seen := agentSessionRelay(t)
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodDelete, "/agent-sessions/" + testSessionID, http.StatusNotFound},
		{http.MethodPost, "/agent-sessions/" + testSessionID + "/tokens", http.StatusNotFound},
		{http.MethodGet, "/agent-sessions/" + testSessionID + "/ssh", http.StatusNotFound},
		{http.MethodPost, "/agent-sessions/" + testSessionID + "/pty-ticket", http.StatusNotFound},
		{http.MethodGet, "/agent-sessions/not-a-uuid/events", http.StatusBadRequest},
		{http.MethodGet, "/agent-preview/short/", http.StatusNotFound},
	}
	for _, c := range cases {
		if resp := relayDo(t, c.method, relay.URL+c.path, nil); resp.StatusCode != c.want { //nolint:bodyclose // helper closes the body
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, relay.URL+"/agent-sessions/"+testSessionID+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer = %d, want 401", resp.StatusCode)
	}
	if len(*seen) != 0 {
		t.Fatalf("FluxVM was called %d times for refused requests", len(*seen))
	}
}

func TestAgentPreviewRelay(t *testing.T) {
	relay, seen := agentSessionRelay(t)
	token := strings.Repeat("ab", 32)
	resp := relayDo(t, http.MethodGet, relay.URL+"/agent-preview/"+token+"/static/app.js?v=2", nil) //nolint:bodyclose // helper closes the body
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := (*seen)[0]
	if got.URL.Path != "/preview/"+token+"/static/app.js" || got.URL.RawQuery != "v=2" {
		t.Fatalf("upstream = %s?%s", got.URL.Path, got.URL.RawQuery)
	}
}
