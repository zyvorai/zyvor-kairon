// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

func TestDownloadCapturePollsUntilReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/machines/default/web/network-capture/tok" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte("pcap-bytes"))
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "secret")

	out := filepath.Join(t.TempDir(), "x.pcap")
	n, err := downloadCapture(context.Background(), "default", "web", ebpfedge.CaptureSession{Token: "tok"}, out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if n != 10 || string(got) != "pcap-bytes" || calls.Load() != 2 {
		t.Fatalf("n=%d got=%q calls=%d", n, got, calls.Load())
	}
}

func TestDownloadCaptureReportsMissing(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	_, err := downloadCapture(context.Background(), "default", "web", ebpfedge.CaptureSession{Token: "tok"}, filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("expected error for 404")
	}
}
