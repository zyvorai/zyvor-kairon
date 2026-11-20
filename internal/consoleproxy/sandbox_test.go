// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package consoleproxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

func TestHandleListSandboxesRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandboxes" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []fluxvm.Record{{UUID: "sandbox-1", Status: "Running"}}})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out struct {
		Items []fluxvm.Record `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].UUID != "sandbox-1" {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestHandleListSandboxesRejectsWrongOrMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHandleListTemplatesRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []fluxvm.TemplateInfo{{Name: "alpine-agent"}}})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/templates", nil)
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

func TestHandleBuildTemplateRelaysRequest(t *testing.T) {
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.TemplateInfo{Name: "alpine-agent"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"name":"alpine-agent","imageRef":"docker.io/library/alpine:3.19"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/templates", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotBody["name"] != "alpine-agent" || gotBody["image_ref"] != "docker.io/library/alpine:3.19" {
		t.Fatalf("unexpected upstream request: %+v", gotBody)
	}
}

func TestHandleSandboxHTTPProxyRelaysRequestAndResponse(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("X-Guest-Header", "yes")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("guest response"))
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sandbox-proxy/sandbox-1/8080/api/webhook", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Guest-Header") != "yes" {
		t.Fatalf("expected the guest's response header to be relayed back, got %+v", resp.Header)
	}
	if gotPath != "/v1/sandboxes/sandbox-1/http/8080/api/webhook" || gotMethod != http.MethodPost || gotBody != "hello" {
		t.Fatalf("unexpected upstream request: path=%q method=%q body=%q", gotPath, gotMethod, gotBody)
	}
}

func TestHandleSandboxHTTPProxyRejectsWrongOrMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sandbox-proxy/sandbox-1/8080/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHandleSandboxHTTPProxyNeverForwardsRelayToken(t *testing.T) {
	var gotAuth string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/sandbox-proxy/sandbox-1/8080/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if gotAuth == "Bearer secret" {
		t.Fatal("expected the kairon-node relay's own shared token never to reach the guest")
	}
}
