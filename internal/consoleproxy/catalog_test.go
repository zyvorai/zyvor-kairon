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

func TestHandleCatalogRoutesRejectMissingToken(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("FluxVM should not be dialed when the token check fails")
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, req := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/catalog", ""},
		{http.MethodPost, "/catalog", `{}`},
		{http.MethodDelete, "/catalog/x", ""},
		{http.MethodPost, "/catalog/x/rename", `{}`},
		{http.MethodPost, "/catalog/x/clone", `{}`},
		{http.MethodPost, "/catalog/x/export", `{}`},
		{http.MethodPost, "/catalog/x/read-only", `{}`},
		{http.MethodPost, "/catalog/clean", ""},
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

func TestHandleListCatalogRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []fluxvm.CatalogListEntry{{CatalogEntry: fluxvm.CatalogEntry{Name: "ubuntu-24.04"}}}})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/catalog", nil)
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

func TestHandleAddCatalogEntryRelaysRequest(t *testing.T) {
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.CatalogEntry{Name: "ubuntu-24.04"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"name":"ubuntu-24.04","source":"https://example.com/u.qcow2","format":"qcow2"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotBody["name"] != "ubuntu-24.04" || gotBody["source"] != "https://example.com/u.qcow2" {
		t.Fatalf("unexpected upstream request: %+v", gotBody)
	}
}

func TestHandleRemoveCatalogEntryRelaysRequest(t *testing.T) {
	var gotPath, gotMethod string
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/catalog/ubuntu-24.04", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected upstream request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestHandleRenameCatalogEntryRelaysRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.CatalogEntry{Name: "ubuntu-2404"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"newName":"ubuntu-2404"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog/ubuntu-24.04/rename", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/rename" || gotBody["new_name"] != "ubuntu-2404" {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestHandleCloneCatalogEntryRelaysRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.CatalogEntry{Name: "ubuntu-24.04-dev"})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"targetName":"ubuntu-24.04-dev"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog/ubuntu-24.04/clone", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/clone" || gotBody["target_name"] != "ubuntu-24.04-dev" {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestHandleExportCatalogEntryRelaysRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"path":"/tmp/exported.qcow2"}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog/ubuntu-24.04/export", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/export" || gotBody["path"] != "/tmp/exported.qcow2" {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestHandleSetCatalogReadOnlyRelaysRequest(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(fluxvm.CatalogEntry{Name: "ubuntu-24.04", ReadOnly: true})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := strings.NewReader(`{"readOnly":true}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog/ubuntu-24.04/read-only", body)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/read-only" || gotBody["read_only"] != true {
		t.Fatalf("unexpected upstream request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestHandleCleanCatalogDownloadsRelaysResponse(t *testing.T) {
	fluxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog/clean" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"removed": 2})
	}))
	defer fluxSrv.Close()
	s := &Server{Flux: fluxvm.New(fluxSrv.URL, ""), Token: "secret"}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/catalog/clean", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["removed"] != float64(2) {
		t.Fatalf("unexpected response: %+v", out)
	}
}
