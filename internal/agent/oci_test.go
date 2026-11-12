// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func tarLayer(t *testing.T, gz bool, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var zw *gzip.Writer
	var tw *tar.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(zw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	if zw != nil {
		_ = zw.Close()
	}
	return buf.Bytes()
}

type fakeRegistry struct {
	srv       *httptest.Server
	blobs     map[string][]byte
	types     map[string]string
	tokenHits atomic.Int32
}

func newFakeRegistry(t *testing.T, repo string) *fakeRegistry {
	r := &fakeRegistry{blobs: map[string][]byte{}, types: map[string]string{}}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/token" {
			r.tokenHits.Add(1)
			if req.URL.Query().Get("scope") != "repository:"+repo+":pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon"})
			return
		}
		if req.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="fake",scope="repository:`+repo+`:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		prefix := "/v2/" + repo + "/"
		rest, ok := strings.CutPrefix(req.URL.Path, prefix)
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, digest, _ := strings.Cut(rest, "/")
		body, ok := r.blobs[digest]
		if !ok {
			http.NotFound(w, req)
			return
		}
		if ct := r.types[digest]; ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) put(body []byte, mediaType string) string {
	d := digestOf(body)
	r.blobs[d] = body
	r.types[d] = mediaType
	return d
}

func (r *fakeRegistry) host() string { return strings.TrimPrefix(r.srv.URL, "https://") }

func (r *fakeRegistry) putManifest(t *testing.T, layers ...[]byte) string {
	descs := make([]ociDescriptor, 0, len(layers))
	for _, l := range layers {
		descs = append(descs, ociDescriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: r.put(l, ""), Size: int64(len(l))})
	}
	b, err := json.Marshal(ociManifest{MediaType: mediaOCIManifest, Layers: descs})
	if err != nil {
		t.Fatal(err)
	}
	return r.put(b, mediaOCIManifest)
}

func TestResolveImageSourcePullsOCIContainerDisk(t *testing.T) {
	reg := newFakeRegistry(t, "containerdisks/alpine")
	disk := append([]byte("QFI\xfb\x00\x00\x00\x03\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), "fake qcow2 disk"...)
	manifest := reg.putManifest(t,
		tarLayer(t, true, map[string][]byte{"disk/old.img": []byte("lower layer")}),
		tarLayer(t, true, map[string][]byte{"etc/x": []byte("x"), "./disk/alpine.qcow2": disk}),
	)
	other, _ := json.Marshal(ociManifest{MediaType: mediaOCIManifest})
	index, _ := json.Marshal(ociManifest{MediaType: mediaOCIIndex, Manifests: []ociDescriptor{
		{MediaType: mediaOCIManifest, Digest: reg.put(other, mediaOCIManifest), Platform: &ociPlatform{OS: "linux", Architecture: "not-" + runtime.GOARCH}},
		{MediaType: mediaOCIManifest, Digest: manifest, Platform: &ociPlatform{OS: "linux", Architecture: runtime.GOARCH}},
	}})
	indexDigest := reg.put(index, mediaOCIIndex)

	a := &Agent{ImageCacheDir: t.TempDir(), RegistryHTTP: reg.srv.Client()}
	m := model.Machine{Spec: model.MachineSpec{Image: model.ImageSpec{
		Source: &model.ImageSource{OCI: reg.host() + "/containerdisks/alpine:3.20"},
		Digest: indexDigest,
	}}}
	got, err := a.resolveImageSource(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveImageSource: %v", err)
	}
	if want := filepath.Join(a.ImageCacheDir, "oci", "sha256", strings.TrimPrefix(indexDigest, "sha256:")); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(got); !bytes.Equal(b, disk) {
		t.Fatalf("cached disk = %q, want the top layer's disk", b)
	}
	if reg.tokenHits.Load() != 1 {
		t.Fatalf("token fetched %d times, want 1", reg.tokenHits.Load())
	}

	reg.srv.Close()
	if again, err := a.resolveImageSource(context.Background(), m); err != nil || again != got {
		t.Fatalf("cached resolve = %q, %v", again, err)
	}
}

func TestPullOCIDiskRejectsTamperedLayer(t *testing.T) {
	reg := newFakeRegistry(t, "d/x")
	layer := tarLayer(t, false, map[string][]byte{"disk/x.img": []byte("raw disk")})
	manifest := reg.putManifest(t, layer)
	for d, b := range reg.blobs {
		if bytes.Equal(b, layer) {
			reg.blobs[d] = tarLayer(t, false, map[string][]byte{"disk/x.img": []byte("evil disk")})
		}
	}
	a := &Agent{ImageCacheDir: t.TempDir(), RegistryHTTP: reg.srv.Client()}
	img := model.ImageSpec{Source: &model.ImageSource{OCI: reg.host() + "/d/x"}, Digest: manifest}
	dest := filepath.Join(a.ImageCacheDir, "disk")
	if err := a.pullOCIDisk(context.Background(), img, dest); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected a layer digest mismatch, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("tampered disk must not reach the cache")
	}
	entries, _ := os.ReadDir(a.ImageCacheDir)
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestPullOCIDiskRejectsWrongManifestDigest(t *testing.T) {
	reg := newFakeRegistry(t, "d/x")
	manifest := reg.putManifest(t, tarLayer(t, false, map[string][]byte{"disk/x.img": []byte("d")}))
	wrong := digestOf([]byte("not the manifest"))
	reg.blobs[wrong] = reg.blobs[manifest]
	a := &Agent{ImageCacheDir: t.TempDir(), RegistryHTTP: reg.srv.Client()}
	img := model.ImageSpec{Source: &model.ImageSource{OCI: reg.host() + "/d/x"}, Digest: wrong}
	if err := a.pullOCIDisk(context.Background(), img, filepath.Join(a.ImageCacheDir, "disk")); err == nil || !strings.Contains(err.Error(), "manifest digest mismatch") {
		t.Fatalf("expected a manifest digest mismatch, got %v", err)
	}
}

func TestPullOCIDiskWithoutDiskDir(t *testing.T) {
	reg := newFakeRegistry(t, "d/x")
	manifest := reg.putManifest(t, tarLayer(t, true, map[string][]byte{"usr/bin/sh": []byte("x")}))
	a := &Agent{ImageCacheDir: t.TempDir(), RegistryHTTP: reg.srv.Client()}
	img := model.ImageSpec{Source: &model.ImageSource{OCI: reg.host() + "/d/x"}, Digest: manifest}
	if err := a.pullOCIDisk(context.Background(), img, filepath.Join(a.ImageCacheDir, "disk")); err == nil || !strings.Contains(err.Error(), "no file under disk/") {
		t.Fatalf("expected a missing-disk error, got %v", err)
	}
}

func TestParseAuthParams(t *testing.T) {
	got := parseAuthParams(`realm="https://auth.example/token",service=registry.example,scope="repository:a/b:pull,push"`)
	if got["realm"] != "https://auth.example/token" || got["service"] != "registry.example" || got["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("parseAuthParams = %v", got)
	}
}
