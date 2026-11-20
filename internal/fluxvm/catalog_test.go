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

func TestListCatalog(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog" || r.Method != http.MethodGet {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		valid := true
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []CatalogListEntry{
			{CatalogEntry: CatalogEntry{Name: "ubuntu-24.04", Source: "/images/ubuntu.qcow2", SHA256: "abc", ReadOnly: true}, SignatureValid: &valid},
		}})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	items, err := c.ListCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "ubuntu-24.04" || !items[0].ReadOnly || items[0].SignatureValid == nil || !*items[0].SignatureValid {
		t.Fatalf("unexpected result: %+v", items)
	}
}

func TestAddCatalogEntry(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(CatalogEntry{Name: "ubuntu-24.04", Source: "https://example.com/ubuntu.qcow2", SHA256: "abc123", Format: "qcow2"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.AddCatalogEntry(context.Background(), "ubuntu-24.04", "https://example.com/ubuntu.qcow2", "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog" || gotBody["name"] != "ubuntu-24.04" || gotBody["source"] != "https://example.com/ubuntu.qcow2" || gotBody["format"] != "qcow2" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if out.SHA256 != "abc123" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestRemoveCatalogEntry(t *testing.T) {
	var gotPath, gotMethod string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.RemoveCatalogEntry(context.Background(), "ubuntu-24.04"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestRemoveCatalogEntryPropagatesReadOnlyRefusal(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "entry is read-only", http.StatusConflict)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.RemoveCatalogEntry(context.Background(), "base-image"); err == nil {
		t.Fatal("expected an error for a read-only entry")
	}
}

func TestRenameCatalogEntry(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(CatalogEntry{Name: "ubuntu-2404"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.RenameCatalogEntry(context.Background(), "ubuntu-24.04", "ubuntu-2404")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/rename" || gotBody["new_name"] != "ubuntu-2404" || out.Name != "ubuntu-2404" {
		t.Fatalf("unexpected request/result: path=%q body=%+v out=%+v", gotPath, gotBody, out)
	}
}

func TestCloneCatalogEntry(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(CatalogEntry{Name: "ubuntu-24.04-dev"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.CloneCatalogEntry(context.Background(), "ubuntu-24.04", "ubuntu-24.04-dev")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/clone" || gotBody["target_name"] != "ubuntu-24.04-dev" || out.Name != "ubuntu-24.04-dev" {
		t.Fatalf("unexpected request/result: path=%q body=%+v out=%+v", gotPath, gotBody, out)
	}
}

func TestExportCatalogEntry(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if err := c.ExportCatalogEntry(context.Background(), "ubuntu-24.04", "/tmp/exported.qcow2"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/export" || gotBody["path"] != "/tmp/exported.qcow2" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
}

func TestSetCatalogReadOnly(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(CatalogEntry{Name: "ubuntu-24.04", ReadOnly: true})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.SetCatalogReadOnly(context.Background(), "ubuntu-24.04", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04/read-only" || gotBody["read_only"] != true || !out.ReadOnly {
		t.Fatalf("unexpected request/result: path=%q body=%+v out=%+v", gotPath, gotBody, out)
	}
}

func TestCleanCatalogDownloads(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog/clean" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"removed": 3})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	removed, err := c.CleanCatalogDownloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("unexpected result: %d", removed)
	}
}
