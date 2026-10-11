// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func imageStoreServer(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()
	s := &Server{
		Users: []User{
			{Username: "root", PasswordHash: hashFor(t, "correct-horse"), IsAdmin: true},
			{Username: "alice", PasswordHash: hashFor(t, "correct-horse")},
		},
		SessionSecret:       []byte("test-session-secret"),
		ImageStoreDir:       t.TempDir(),
		ImageStorePublicURL: "http://kairon-ui.kairon-system.svc:8082/",
		ImageStoreMaxBytes:  1024,
	}
	h := s.Handler()
	return s, h, loginToken(t, h, "root", "correct-horse"), loginToken(t, h, "alice", "correct-horse")
}

func putImage(h http.Handler, path, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestImageStoreUploadServeDelete(t *testing.T) {
	s, h, admin, _ := imageStoreServer(t)
	body := []byte("QFI\xfb tiny qcow2")
	sum := sha256.Sum256(body)
	hexDigest := hex.EncodeToString(sum[:])

	rr := putImage(h, "/api/v1/images/alpine-3.20?format=qcow2", admin, body, map[string]string{"X-Image-Digest": "sha256:" + hexDigest})
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var img UploadedImage
	if err := json.Unmarshal(rr.Body.Bytes(), &img); err != nil {
		t.Fatal(err)
	}
	if img.Digest != "sha256:"+hexDigest || img.SizeBytes != int64(len(body)) || img.Format != "qcow2" || img.UploadedBy != "root" {
		t.Fatalf("unexpected entry %+v", img)
	}
	if img.URL != "http://kairon-ui.kairon-system.svc:8082/images/sha256/"+hexDigest {
		t.Fatalf("url = %q", img.URL)
	}

	if rr := putImage(h, "/api/v1/images/alpine-3.20", admin, body, nil); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", rr.Code)
	}
	if rr := putImage(h, "/api/v1/images/alias", admin, body, nil); rr.Code != http.StatusCreated {
		t.Fatalf("alias upload: %d %s", rr.Code, rr.Body.String())
	}

	// Blobs are served without credentials, with range support.
	req := httptest.NewRequest(http.MethodGet, "/images/sha256/"+hexDigest, nil)
	req.Header.Set("Range", "bytes=0-3")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusPartialContent || got.Body.String() != "QFI\xfb" {
		t.Fatalf("blob range: %d %q", got.Code, got.Body.String())
	}

	list := doJSON(t, h, http.MethodGet, "/api/v1/images", admin, nil)
	var imgs []UploadedImage
	_ = json.Unmarshal(list.Body.Bytes(), &imgs)
	if len(imgs) != 2 {
		t.Fatalf("list = %s", list.Body.String())
	}

	blob := filepath.Join(s.ImageStoreDir, "sha256", hexDigest)
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/images/alpine-3.20", admin, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatal("blob removed while another name still points at it")
	}
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/images/alias", admin, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("delete alias: %d", rr.Code)
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatal("unreferenced blob should be removed")
	}
	if rr := doJSON(t, h, http.MethodDelete, "/api/v1/images/alias", admin, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", rr.Code)
	}
}

func TestImageStoreRejects(t *testing.T) {
	s, h, admin, alice := imageStoreServer(t)
	cases := []struct {
		name, path, token string
		body              []byte
		hdr               map[string]string
		want              int
	}{
		{"non-admin", "/api/v1/images/x", alice, []byte("d"), nil, http.StatusForbidden},
		{"bad name", "/api/v1/images/Bad_Name", admin, []byte("d"), nil, http.StatusBadRequest},
		{"bad format", "/api/v1/images/x?format=iso", admin, []byte("d"), nil, http.StatusBadRequest},
		{"empty", "/api/v1/images/x", admin, nil, nil, http.StatusBadRequest},
		{"too big", "/api/v1/images/x", admin, bytes.Repeat([]byte("a"), 2048), nil, http.StatusRequestEntityTooLarge},
		{"digest mismatch", "/api/v1/images/x", admin, []byte("d"), map[string]string{"X-Image-Digest": "sha256:00"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if rr := putImage(h, c.path, c.token, c.body, c.hdr); rr.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", c.name, rr.Code, c.want, rr.Body.String())
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.ImageStoreDir, "sha256"))
	if len(entries) != 0 {
		t.Fatalf("rejected uploads left files: %v", entries)
	}
	for _, p := range []string{"/images/sha256/../../etc/passwd", "/images/sha256/abc"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code == http.StatusOK {
			t.Errorf("GET %s should not succeed", p)
		}
	}
}

func TestImageStoreDisabled(t *testing.T) {
	s := &Server{Token: "t"}
	h := s.Handler()
	if rr := doJSON(t, h, http.MethodGet, "/api/v1/images", "t", nil); rr.Code != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501", rr.Code)
	}
}

// With kairon-ui serving a self-signed certificate, nodes download the blob
// over that listener, so the upload response marks the source
// insecureSkipTLSVerify (the pinned sha256 still guarantees the bytes).
func TestImageStoreMarksSelfSignedHTTPSSources(t *testing.T) {
	cases := []struct {
		name       string
		publicURL  string
		selfSigned bool
		want       bool
	}{
		{"https and self-signed", "https://kairon-ui.kairon-system.svc:18082", true, true},
		{"https with a trusted certificate", "https://kairon-ui.example.com", false, false},
		{"plain http (opt-out) is never marked", "http://kairon-ui.kairon-system.svc:18082", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, h, admin, _ := imageStoreServer(t)
			s.ImageStorePublicURL = c.publicURL
			s.TLSSelfSigned = c.selfSigned
			rr := putImage(h, "/api/v1/images/probe", admin, []byte("QFI\xfb x"), nil)
			if rr.Code != http.StatusCreated {
				t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
			}
			var img UploadedImage
			if err := json.Unmarshal(rr.Body.Bytes(), &img); err != nil {
				t.Fatal(err)
			}
			if img.InsecureSkipTLSVerify != c.want {
				t.Errorf("InsecureSkipTLSVerify = %v, want %v (url %s)", img.InsecureSkipTLSVerify, c.want, img.URL)
			}
		})
	}
}
