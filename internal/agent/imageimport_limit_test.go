// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadToTempEnforcesSizeLimit(t *testing.T) {
	// Chunked (unknown length) body longer than the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
			f.Flush()
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "img")
	err := downloadToTemp(context.Background(), srv.URL, dst, strings.Repeat("0", 64), false, 4096)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected a size-limit error, got %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("no partial/temp file may remain, found %d entries", len(ents))
	}
}

func TestDownloadToTempRejectsOversizeContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	defer srv.Close()
	err := downloadToTemp(context.Background(), srv.URL, filepath.Join(t.TempDir(), "img"), strings.Repeat("0", 64), false, 100)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("expected a size-limit error, got %v", err)
	}
}
