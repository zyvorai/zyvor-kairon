// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestHandleListCatalogFullRelay(t *testing.T) {
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"name":"ubuntu-24.04"}]}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodGet, "/api/v1/nodes/worker-1/catalog", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAddCatalogEntryDeniesNonAdmin(t *testing.T) {
	fk := newFakeKube()
	fk.nodes = []model.Node{{Metadata: model.ObjectMeta{Name: "worker-1"}}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()
	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "alice", PasswordHash: hashFor(t, "correct-horse")}}, // not admin
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  "x", ConsolePort: "8090",
	}
	h := s.Handler()
	token := loginToken(t, h, "alice", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/catalog", token, addCatalogEntryRequest{Name: "ubuntu-24.04", Source: "https://example.com/u.qcow2"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-admin account, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleAddCatalogEntryFullRelay(t *testing.T) {
	var gotBody string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"name":"ubuntu-24.04","source":"https://example.com/u.qcow2","sha256":"abc"}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/catalog", token, addCatalogEntryRequest{Name: "ubuntu-24.04", Source: "https://example.com/u.qcow2"})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotBody == "" {
		t.Fatal("expected the request to reach FluxVM through both relay hops")
	}
}

func TestHandleRemoveCatalogEntryFullRelay(t *testing.T) {
	var gotPath, gotMethod string
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodDelete, "/api/v1/nodes/worker-1/catalog/ubuntu-24.04", token, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/images/catalog/ubuntu-24.04" || gotMethod != http.MethodDelete {
		t.Fatalf("unexpected upstream request: method=%q path=%q", gotMethod, gotPath)
	}
}

func TestHandleCleanCatalogDownloadsFullRelay(t *testing.T) {
	nodeHost, nodePort, relayToken, cleanup := nodeRelaySetup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/images/catalog/clean" {
			http.Error(w, "bad route", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"removed":1}`))
	})
	defer cleanup()

	fk := newFakeKube()
	fk.nodes = []model.Node{{
		Metadata: model.ObjectMeta{Name: "worker-1"},
		Status:   model.NodeStatus{Addresses: []model.NodeAddress{{Type: "InternalIP", Address: nodeHost}}},
	}}
	kubeSrv := httptest.NewServer(fk.handler())
	defer kubeSrv.Close()

	s := &Server{
		Kube:          mustKubeClientAt(t, kubeSrv.URL),
		Users:         []User{{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true}},
		SessionSecret: []byte("test-session-secret"),
		ConsoleToken:  relayToken, ConsolePort: nodePort,
	}
	h := s.Handler()
	token := loginToken(t, h, "root", "correct-horse")

	rr := doJSON(t, h, http.MethodPost, "/api/v1/nodes/worker-1/catalog/clean", token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
