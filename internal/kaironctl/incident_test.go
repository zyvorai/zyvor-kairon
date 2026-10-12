// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
)

func incidentKube(t *testing.T) *kube.Client {
	t.Helper()
	machine := `{"apiVersion":"kairon.zyvor.dev/v1","kind":"Machine",
		"metadata":{"name":"web","namespace":"default","uid":"u-1"},
		"spec":{"cloudInit":{"userData":"#cloud-config\npassword: hunter2"},"resources":{"cpu":"1"}},
		"status":{"phase":"Running","nodeName":"n1","runtimeID":"rt-1"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web":
			_, _ = w.Write([]byte(machine))
		case "/api/v1/nodes/n1":
			_, _ = w.Write([]byte(`{"metadata":{"name":"n1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`))
		default:
			_, _ = w.Write([]byte(`{"items":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

func nodeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(data))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func readTarGz(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

// fakeIncidentUI answers kairon-ui's diagnostics routes; sha overrides the
// sha256 the job reports.
func fakeIncidentUI(t *testing.T, bundle []byte, sha string) *httptest.Server {
	t.Helper()
	if sha == "" {
		sum := sha256.Sum256(bundle)
		sha = hex.EncodeToString(sum[:])
	}
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/api/v1/machines/default/web/diagnostics"
		switch r.Method + " " + r.URL.Path {
		case "POST " + base:
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["memory_dump"] != true || req["redact"] != true || req["pcap_seconds"] != float64(5) {
				t.Errorf("request %v", req)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"token":"tok","state":"running"}`))
		case "GET " + base + "/tok":
			polls++
			state := "running"
			if polls > 1 {
				state = "done"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "tok", "state": state, "sha256": sha, "size_bytes": len(bundle),
				"memory_dump": map[string]any{"path": "/var/lib/fluxvm/diagnostics/x/tok/memory.elf", "format": "elf", "size_bytes": 1 << 30, "sha256": "dd", "paused_ms": 1200},
			})
		case "GET " + base + "/tok/bundle":
			_, _ = w.Write(bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWriteIncidentCombinesClusterAndNode(t *testing.T) {
	incidentPoll = time.Millisecond
	bundle := nodeTarGz(t, map[string]string{
		"manifest.json":         `{"token":"tok"}`,
		"qmp/query-status.json": `{"status":"running"}`,
	})
	srv := fakeIncidentUI(t, bundle, "")
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "")

	var out bytes.Buffer
	o := &incidentOpts{Deep: true, Pcap: 5 * time.Second, MemoryDump: true}
	m, err := writeIncident(context.Background(), incidentKube(t), &out, "default", "web", o, "test")
	if err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, out.Bytes())
	for _, want := range []string{"cluster/machine.json", "cluster/events.json", "cluster/migrations.json", "cluster/diagnosis.json", "cluster/node.json", "node/manifest.json", "node/qmp/query-status.json", "manifest.json", "README.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s (have %v)", want, keys(files))
		}
	}
	if strings.Contains(files["cluster/machine.json"], "hunter2") {
		t.Fatalf("cloudInit not redacted: %s", files["cluster/machine.json"])
	}
	if m.Node != "n1" || m.RuntimeID != "rt-1" || m.NodeBundle == nil || m.NodeBundle.Token != "tok" {
		t.Fatalf("manifest %+v", m)
	}
	if !strings.Contains(m.MemoryDump, "kaironctl diagnose memory-dump web tok") {
		t.Fatalf("fetch hint %q", m.MemoryDump)
	}
	if len(m.Skipped) != 0 {
		t.Fatalf("skipped %v", m.Skipped)
	}
}

func TestWriteIncidentRejectsShaMismatch(t *testing.T) {
	incidentPoll = time.Millisecond
	srv := fakeIncidentUI(t, nodeTarGz(t, map[string]string{"manifest.json": "{}"}), strings.Repeat("0", 64))
	t.Setenv("KAIRON_UI_URL", srv.URL)
	var out bytes.Buffer
	o := &incidentOpts{Deep: true, Pcap: 5 * time.Second, MemoryDump: true}
	m, err := writeIncident(context.Background(), incidentKube(t), &out, "default", "web", o, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(m.Skipped, "\n"), "sha256") {
		t.Fatalf("skipped %v", m.Skipped)
	}
	for name := range readTarGz(t, out.Bytes()) {
		if strings.HasPrefix(name, "node/") {
			t.Fatalf("unverified node file %s in the bundle", name)
		}
	}
}

func TestWriteIncidentWithoutUIKeepsClusterSide(t *testing.T) {
	t.Setenv("KAIRON_UI_URL", "")
	var out bytes.Buffer
	m, err := writeIncident(context.Background(), incidentKube(t), &out, "default", "web", &incidentOpts{Deep: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, out.Bytes())
	if _, ok := files["cluster/machine.json"]; !ok {
		t.Fatal("no cluster/machine.json")
	}
	if len(m.Skipped) != 1 || !strings.Contains(m.Skipped[0], "KAIRON_UI_URL") {
		t.Fatalf("skipped %v", m.Skipped)
	}
}

func TestWriteIncidentNoRedactKeepsCloudInit(t *testing.T) {
	t.Setenv("KAIRON_UI_URL", "")
	var out bytes.Buffer
	if _, err := writeIncident(context.Background(), incidentKube(t), &out, "default", "web", &incidentOpts{Deep: true, NoRedact: true}, "test"); err != nil {
		t.Fatal(err)
	}
	files := readTarGz(t, out.Bytes())
	if !strings.Contains(files["cluster/machine.json"], "hunter2") {
		t.Fatal("--no-redact dropped cloudInit")
	}
	if !strings.Contains(files["README.txt"], "NOT redacted") {
		t.Fatalf("README %q", files["README.txt"])
	}
}

func TestDownloadMemoryDumpChecksSha(t *testing.T) {
	data := []byte("\x7fELF core")
	sum := sha256.Sum256(data)
	good := hex.EncodeToString(sum[:])
	for _, c := range []struct {
		sha     string
		wantErr bool
	}{{good, false}, {strings.Repeat("0", 64), true}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/machines/default/db/diagnostics/tok":
				_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "state": "done", "memory_dump": map[string]any{"sha256": c.sha}})
			case "/api/v1/machines/default/db/diagnostics/tok/memory-dump":
				_, _ = w.Write(data)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Setenv("KAIRON_UI_URL", srv.URL)
		out := filepath.Join(t.TempDir(), "db.elf")
		n, got, err := downloadMemoryDump(context.Background(), "default", "db", "tok", out)
		srv.Close()
		if (err != nil) != c.wantErr {
			t.Fatalf("sha %s: err=%v", c.sha[:4], err)
		}
		if !c.wantErr {
			raw, _ := os.ReadFile(out)
			if n != int64(len(data)) || got != good || !bytes.Equal(raw, data) {
				t.Fatalf("n=%d sum=%s", n, got)
			}
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
