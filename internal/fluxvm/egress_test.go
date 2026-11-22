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

func TestEgressCheck(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(EgressDecision{Allow: true, Reason: "host matched allowlist", InjectAuthorization: "Bearer secret-vault-token"})
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	out, err := c.EgressCheck(context.Background(), "api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/egress/check" || gotBody["host"] != "api.example.com" {
		t.Fatalf("unexpected request: path=%q body=%+v", gotPath, gotBody)
	}
	if !out.Allow || out.Reason != "host matched allowlist" || out.InjectAuthorization != "Bearer secret-vault-token" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestEgressCheckPropagatesErrors(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer s.Close()
	c := New(s.URL, "")
	c.HTTP = s.Client()

	if _, err := c.EgressCheck(context.Background(), "x"); err == nil {
		t.Fatal("expected an error when the server rejects the request")
	}
}
