// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeadersSet(t *testing.T) {
	s := &Server{}
	h := s.withSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "SAMEORIGIN",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent over plaintext")
	}
}

func TestSessionSubjectStillValid(t *testing.T) {
	s := &Server{Users: []User{{Username: "alice"}}}
	if !s.sessionSubjectStillValid("alice") {
		t.Error("configured local user must stay valid")
	}
	if s.sessionSubjectStillValid("bob") {
		t.Error("a user absent from Users must not keep a valid session")
	}
	if !s.sessionSubjectStillValid(oidcSubjectPrefix + "carol@example.com") {
		t.Error("OIDC subjects are valid regardless of local users")
	}
	// An OIDC login named like a local admin must not map onto that account.
	s.Users = []User{{Username: "admin", IsAdmin: true}}
	if u, found := s.findUser(oidcSubjectPrefix + "admin"); found {
		t.Errorf("OIDC subject resolved to local user %+v", u)
	}
}

func TestViewerCannotTunnelOrDownloadDisks(t *testing.T) {
	s := &Server{Users: []User{{Username: "v", Role: "viewer"}}}
	h := s.withActionAuthorization(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	for path, want := range map[string]int{
		"/api/v1/machines":                   http.StatusOK,
		"/api/v1/machines/ns/vm/logs":        http.StatusOK,
		"/api/v1/machines/ns/vm/portforward": http.StatusForbidden,
		"/api/v1/exports/ns/vm/download":     http.StatusForbidden,
	} {
		user := "v"
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), usernameHolderKey, &user))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s as viewer = %d, want %d", path, rec.Code, want)
		}
	}
}
