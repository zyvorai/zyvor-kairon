// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
)

func atlasFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer atlas-secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/atlas/v1/volumes":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
				{"id": "a", "owner": "kairon/machine/default/web"},
				{"id": "b", "owner": "kryton/vm/x"},
			}})
		case "/api/atlas/v1/pools":
			_, _ = w.Write([]byte(`[{"name":"rbd"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func atlasGet(s *Server, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer ui-token")
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAtlasProxyFiltersVolumesAndHidesToken(t *testing.T) {
	up := atlasFake(t)
	defer up.Close()
	s := &Server{Token: "ui-token", AtlasURL: up.URL, AtlasToken: "atlas-secret"}

	rr := atlasGet(s, "/api/v1/atlas/volumes")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0]["id"] != "a" {
		t.Fatalf("expected only the kairon-owned volume, got %v", out.Items)
	}
	if rr := atlasGet(s, "/api/v1/atlas/pools"); rr.Code != http.StatusOK {
		t.Fatalf("pools: %d", rr.Code)
	}
}

func TestAtlasProxyAllowlistAndUnconfigured(t *testing.T) {
	up := atlasFake(t)
	defer up.Close()
	s := &Server{Token: "ui-token", AtlasURL: up.URL, AtlasToken: "atlas-secret"}
	if rr := atlasGet(s, "/api/v1/atlas/auth/tokens"); rr.Code != http.StatusNotFound {
		t.Fatalf("non-allowlisted path must 404, got %d", rr.Code)
	}
	off := &Server{Token: "ui-token"}
	if rr := atlasGet(off, "/api/v1/atlas/pools"); rr.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured must 501, got %d", rr.Code)
	}
	bad := &Server{Token: "ui-token", AtlasURL: up.URL, AtlasToken: "wrong"}
	if rr := atlasGet(bad, "/api/v1/atlas/pools"); rr.Code != http.StatusBadGateway {
		t.Fatalf("upstream 401 must surface as 502, got %d", rr.Code)
	}
}

func TestConfigReportsAtlas(t *testing.T) {
	s := &Server{Token: "ui-token", AtlasURL: "http://x", AtlasConsoleURL: "http://console"}
	rr := atlasGet(s, "/api/v1/config")
	var c map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c["atlasEnabled"] != true || c["atlasConsoleURL"] != "http://console" {
		t.Fatalf("config = %v", c)
	}
}

func TestBackupAndPoolListRoutesReturnItems(t *testing.T) {
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"x"}}]}`))
	}))
	defer api.Close()
	kc, err := kube.New(api.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Token: "ui-token", Kube: kc}
	for _, p := range []string{"machinepools", "machineclaims", "machinebackups", "machinebackuprestores"} {
		rr := atlasGet(s, "/api/v1/"+p)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", p, rr.Code, rr.Body)
		}
		var items []map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil || len(items) != 1 {
			t.Fatalf("%s: body %s (%v)", p, rr.Body, err)
		}
	}
	if len(paths) != 4 || paths[0] != "/apis/kairon.zyvor.dev/v1/namespaces/default/machinepools" {
		t.Fatalf("unexpected upstream paths: %v", paths)
	}
}
