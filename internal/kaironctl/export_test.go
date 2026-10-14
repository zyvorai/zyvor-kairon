// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestDownloadExportResumesAPartialFile(t *testing.T) {
	const disk = "0123456789"
	var sawRange, sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange, sawAuth = r.Header.Get("Range"), r.Header.Get("Authorization")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(disk))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(out, []byte("0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := downloadExport(context.Background(), srv.Client(), srv.URL, "tok", out, int64(len(disk)))
	if err != nil || n != int64(len(disk)) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if sawRange != "bytes=4-" || sawAuth != "Bearer tok" {
		t.Fatalf("range=%q auth=%q", sawRange, sawAuth)
	}
	if b, _ := os.ReadFile(out); string(b) != disk {
		t.Fatalf("file %q", b)
	}
	sawRange = "unset"
	if n, err := downloadExport(context.Background(), srv.Client(), srv.URL, "tok", out, int64(len(disk))); err != nil || n != int64(len(disk)) || sawRange != "unset" {
		t.Fatalf("complete file re-downloaded: n=%d err=%v range=%q", n, err, sawRange)
	}
}

func TestWriteExportTable(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	later, earlier := now.Add(90*time.Minute), now.Add(-time.Minute)
	items := []model.MachineExport{
		{Metadata: model.ObjectMeta{Name: "web-b"}, Spec: model.MachineExportSpec{MachineName: "web"}, Status: model.MachineExportStatus{Phase: "Expired", ExpiresAt: &earlier}},
		{Metadata: model.ObjectMeta{Name: "web-a"}, Spec: model.MachineExportSpec{MachineName: "web"}, Status: model.MachineExportStatus{Phase: "Ready", Format: "qcow2", SizeBytes: 3 << 30, ExpiresAt: &later, NodeName: "w1"}},
	}
	var buf strings.Builder
	writeExportTable(&buf, items, now)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "web-a") {
		t.Fatalf("table:\n%s", buf.String())
	}
	for _, want := range []string{"3.0 GiB", "in 1h30m0s", "qcow2", "w1"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("row %q lacks %q", lines[1], want)
		}
	}
	if !strings.Contains(lines[2], "expired") {
		t.Fatalf("row %q", lines[2])
	}
	buf.Reset()
	writeExportTable(&buf, nil, now)
	if !strings.Contains(buf.String(), "No MachineExports") {
		t.Fatalf("empty: %q", buf.String())
	}
}

func TestDownloadExportRestartsWhenServerIgnoresRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fresh"))
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(out, []byte("stale-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadExport(context.Background(), srv.Client(), srv.URL, "", out, 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "fresh" {
		t.Fatalf("file %q", b)
	}
}
