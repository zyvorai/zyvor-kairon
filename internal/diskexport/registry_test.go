// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package diskexport

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeSupportsRangeAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Registry{}
	r.Put("prod/disk", Entry{Path: path, Format: "raw", Expires: time.Now().Add(time.Hour)})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Range", "bytes=4-")
	rec := httptest.NewRecorder()
	r.Serve(rec, req, "prod/disk")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "456789" || rec.Header().Get("X-Kairon-Disk-Format") != "raw" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), "prod/other")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key: %d", rec.Code)
	}

	r.Put("prod/disk", Entry{Path: path, Expires: time.Now().Add(-time.Second)})
	rec = httptest.NewRecorder()
	r.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), "prod/disk")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expired: %d", rec.Code)
	}

	var nilReg *Registry
	rec = httptest.NewRecorder()
	nilReg.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), "prod/disk")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil registry: %d", rec.Code)
	}
}
