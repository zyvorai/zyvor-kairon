// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFork(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"items":[{"id":"c1","name":"kid-1","status":"Running"},{"id":"c2","name":"kid-2","status":"Running"}],"elapsed_ms":41}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	kids, err := c.Fork(context.Background(), "vm-1", 2, "kid")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/vms/vm-1/fork" || gotBody["count"] != float64(2) || gotBody["namePrefix"] != "kid" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if len(kids) != 2 || kids[0].ID() != "c1" || kids[1].Name != "kid-2" {
		t.Fatalf("children = %+v", kids)
	}
}

func TestForkRejectsShortResponse(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()
	if _, err := c.Fork(context.Background(), "vm-1", 1, ""); err == nil {
		t.Fatal("want an error when FluxVM returns fewer children than asked for")
	}
}
