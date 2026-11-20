// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageUploadPrintsSpec(t *testing.T) {
	body := []byte("QFI\xfb disk bytes")
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/images/noble" || r.URL.Query().Get("format") != "qcow2" {
			http.Error(w, `{"error":"bad request `+r.URL.String()+`"}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer admin-session" || r.Header.Get("X-Image-Digest") != digest || r.ContentLength != int64(len(body)) {
			http.Error(w, `{"error":"bad headers"}`, http.StatusBadRequest)
			return
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != string(body) {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(uploadedImage{Name: "noble", Digest: digest, SizeBytes: int64(len(body)), Format: "qcow2", URL: "http://ui/images/sha256/" + digest[7:]})
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	t.Setenv("KAIRON_UI_TOKEN", "admin-session")

	file := filepath.Join(t.TempDir(), "Noble.qcow2")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newImageCmd(&Options{})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"upload", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	want := "image:\n  source:\n    httpURL: http://ui/images/sha256/" + digest[7:] + "\n    format: qcow2\n  digest: " + digest + "\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestImageUploadSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"uploading images requires an admin account"}`))
	}))
	defer srv.Close()
	t.Setenv("KAIRON_UI_URL", srv.URL)
	file := filepath.Join(t.TempDir(), "x.raw")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := uploadImage(t.Context(), file, "x", "raw", false); err == nil || !strings.Contains(err.Error(), "admin account") {
		t.Fatalf("expected the server's error, got %v", err)
	}
}

func TestImageUploadNeedsUIURL(t *testing.T) {
	t.Setenv("KAIRON_UI_URL", "")
	if _, err := uploadImage(t.Context(), "/nonexistent", "x", "", false); err == nil || !strings.Contains(err.Error(), "KAIRON_UI_URL") {
		t.Fatalf("got %v", err)
	}
}

func TestImageNameAndFormatDefaults(t *testing.T) {
	if got := defaultImageName("/tmp/Ubuntu-24.04.QCOW2"); got != "ubuntu-24.04" {
		t.Errorf("defaultImageName = %q", got)
	}
	for file, want := range map[string]string{"a.qcow2": "qcow2", "a.OVA": "ova", "a.img": "", "a": ""} {
		if got := formatFromExt(file); got != want {
			t.Errorf("formatFromExt(%q) = %q, want %q", file, got, want)
		}
	}
}
