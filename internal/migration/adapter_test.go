// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package migration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func serveAdapter(t *testing.T, h http.Handler) *Adapter {
	t.Helper()
	dir, err := os.MkdirTemp("", "kad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewAdapter(sock)
}

func TestAdapterCapabilitiesTreatsMissingRouteAsNone(t *testing.T) {
	a := serveAdapter(t, http.NotFoundHandler())
	caps, err := a.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Tune || caps.PostCopySwitch || caps.Finish {
		t.Fatalf("caps = %+v, want none", caps)
	}
}

func TestAdapterTunePostCopyAndFinishRoutes(t *testing.T) {
	var paths []string
	var tune TuneRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Capabilities{Tune: true, Compression: []string{"zstd"}})
	})
	mux.HandleFunc("POST /v1/source/{s}/{t}/{action}", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.PathValue("action") == "tune" {
			_ = json.NewDecoder(r.Body).Decode(&tune)
		}
		_ = json.NewEncoder(w).Encode(TransferStatus{Phase: "running", PostCopyActive: r.PathValue("action") == "postcopy"})
	})
	a := serveAdapter(t, mux)
	ctx := context.Background()
	s := Session{ID: "sess-1"}
	caps, err := a.Capabilities(ctx)
	if err != nil || !caps.Tune || !caps.SupportsCompression("zstd") || caps.SupportsCompression("zlib") {
		t.Fatalf("caps = %+v err=%v", caps, err)
	}
	src := SourceAdapter{Adapter: a}
	var _ Tuner = src
	var _ Finisher = src
	if _, err := src.Tune(ctx, s, "x1", TuneRequest{MaxCPUThrottlePct: 30}); err != nil {
		t.Fatal(err)
	}
	st, err := src.StartPostCopy(ctx, s, "x1")
	if err != nil || !st.PostCopyActive {
		t.Fatalf("postcopy = %+v err=%v", st, err)
	}
	if err := src.Finish(ctx, s, "x1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/v1/source/sess-1/x1/tune", "/v1/source/sess-1/x1/postcopy", "/v1/source/sess-1/x1/finish"}
	for i := range want {
		if i >= len(paths) || paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
	if tune.MaxCPUThrottlePct != 30 {
		t.Fatalf("tune = %+v", tune)
	}
}

func TestTelemetryDirtyRateMbps(t *testing.T) {
	if got := (Telemetry{DirtyPagesRate: 25000, PageSize: 4096}).DirtyRateMbps(); got < 819 || got > 820 {
		t.Fatalf("DirtyRateMbps = %v, want ~819.2", got)
	}
	if (Telemetry{DirtyPagesRate: 1}).DirtyRateMbps() != 0 {
		t.Fatal("unknown page size must give 0")
	}
}
