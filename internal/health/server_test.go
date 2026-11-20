// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadiness(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", w.Code)
	}
	s.SetReady(true)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d", w.Code)
	}
}
