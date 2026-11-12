// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return model.ImageDigestPrefix + hex.EncodeToString(sum[:])
}

func machineWithImageSource(url, digest string) model.Machine {
	return model.Machine{
		Spec: model.MachineSpec{
			Image: model.ImageSpec{
				Source: &model.ImageSource{HTTPURL: url},
				Digest: digest,
			},
		},
	}
}

func TestResolveImageSourceRequiresCacheDir(t *testing.T) {
	a := &Agent{}
	m := machineWithImageSource("http://example.invalid/x.qcow2", digestOf([]byte("x")))
	if _, err := a.resolveImageSource(context.Background(), m); err == nil {
		t.Fatal("expected an error when ImageCacheDir is unset")
	}
}

func TestValidateImageSourceRequiresURLAndDigest(t *testing.T) {
	validDigest := digestOf([]byte("payload"))
	cases := []struct {
		name string
		img  model.ImageSpec
		ok   bool
	}{
		{"no source", model.ImageSpec{Path: "/x"}, true},
		{"missing url", model.ImageSpec{Source: &model.ImageSource{}, Digest: validDigest}, false},
		{"non-http url", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "ftp://x/y"}, Digest: validDigest}, false},
		{"missing digest", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "http://x/y"}}, false},
		{"malformed digest", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "http://x/y"}, Digest: "sha256:short"}, false},
		{"valid", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "http://x/y"}, Digest: validDigest}, true},
		{"ova format", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "http://x/y.ova", Format: "ova", Repair: true}, Digest: validDigest}, true},
		{"bad format", model.ImageSpec{Source: &model.ImageSource{HTTPURL: "http://x/y", Format: "iso"}, Digest: validDigest}, false},
		{"oci", model.ImageSpec{Source: &model.ImageSource{OCI: "quay.io/containerdisks/fedora:40"}, Digest: validDigest}, true},
		{"oci pinned", model.ImageSpec{Source: &model.ImageSource{OCI: "quay.io/containerdisks/fedora@" + validDigest}, Digest: validDigest}, true},
		{"oci pin mismatch", model.ImageSpec{Source: &model.ImageSource{OCI: "quay.io/containerdisks/fedora@" + digestOf([]byte("other"))}, Digest: validDigest}, false},
		{"oci and url", model.ImageSpec{Source: &model.ImageSource{OCI: "quay.io/x/y", HTTPURL: "http://x/y"}, Digest: validDigest}, false},
		{"oci scheme", model.ImageSpec{Source: &model.ImageSource{OCI: "docker://quay.io/x/y"}, Digest: validDigest}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := model.ValidateImageSource(c.img)
			if c.ok && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestResolveImageSourceDownloadsAndCaches(t *testing.T) {
	payload := []byte("fake qcow2 bytes")
	wantDigest := digestOf(payload)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	a := &Agent{ImageCacheDir: t.TempDir()}
	m := machineWithImageSource(srv.URL, wantDigest)

	path, err := a.resolveImageSource(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveImageSource: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached file: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("cached file contents mismatch: got %q want %q", got, payload)
	}
	if requests != 1 {
		t.Fatalf("expected 1 request, got %d", requests)
	}

	// Second call with the same digest must not re-download.
	path2, err := a.resolveImageSource(context.Background(), m)
	if err != nil {
		t.Fatalf("resolveImageSource (cached): %v", err)
	}
	if path2 != path {
		t.Fatalf("expected the same cache path, got %q and %q", path, path2)
	}
	if requests != 1 {
		t.Fatalf("expected still 1 request after a cache hit, got %d", requests)
	}
}

func TestResolveImageSourceRejectsDigestMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("actual bytes"))
	}))
	defer srv.Close()

	wrongDigest := digestOf([]byte("some other bytes"))
	a := &Agent{ImageCacheDir: t.TempDir()}
	m := machineWithImageSource(srv.URL, wrongDigest)

	if _, err := a.resolveImageSource(context.Background(), m); err == nil {
		t.Fatal("expected a digest mismatch error")
	}
	hexDigest := wrongDigest[len(model.ImageDigestPrefix):]
	if _, err := os.Stat(filepath.Join(a.ImageCacheDir, "sha256", hexDigest)); !os.IsNotExist(err) {
		t.Fatalf("expected no file left behind after a digest mismatch, stat error: %v", err)
	}
}

func TestResolveImageSourcePropagatesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := &Agent{ImageCacheDir: t.TempDir()}
	m := machineWithImageSource(srv.URL, digestOf([]byte("x")))
	if _, err := a.resolveImageSource(context.Background(), m); err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}

func TestResolveImageSourceRejectsMalformedSpec(t *testing.T) {
	a := &Agent{ImageCacheDir: t.TempDir()}
	m := machineWithImageSource("not-a-url", "sha256:bad")
	if _, err := a.resolveImageSource(context.Background(), m); err == nil {
		t.Fatal("expected validation to reject a non-http URL and malformed digest")
	}
}

func TestNeedsImport(t *testing.T) {
	for _, c := range []struct {
		src  model.ImageSource
		want bool
	}{
		{model.ImageSource{}, false},
		{model.ImageSource{Format: "qcow2"}, false},
		{model.ImageSource{Format: "raw", Repair: true}, true},
		{model.ImageSource{Format: "ova"}, true},
		{model.ImageSource{Format: "vmdk"}, true},
	} {
		if got := c.src.NeedsImport(); got != c.want {
			t.Errorf("%+v: NeedsImport = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestResolveImportedImageCallsFluxVMOncePerDigest(t *testing.T) {
	var calls atomic.Int32
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/import" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"image":"/var/lib/fluxvm/images/imported/x/disk0.raw","extra_disks":["/var/lib/fluxvm/images/imported/x/disk1.raw"],"repair":{"os_type":"linux","distro":"ubuntu","actions":["disabled open-vm-tools.service"],"warnings":[]}}`)
	}))
	defer srv.Close()
	a := &Agent{ImageCacheDir: t.TempDir(), Flux: fluxvm.New(srv.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := machineWithImageSource("http://x/web01.ova", digestOf([]byte("ova")))
	m.Spec.Image.Source.Format = "ova"
	m.Spec.Image.Source.Repair = true

	for range 2 {
		got, err := a.resolveImportedImage(context.Background(), m, "/cache/sha256/abc")
		if err != nil {
			t.Fatal(err)
		}
		disks := got.dataDisks()
		if got.Image != "/var/lib/fluxvm/images/imported/x/disk0.raw" || len(disks) != 1 ||
			disks[0] != (fluxvm.DataDisk{Name: "import-disk1", Backing: "/var/lib/fluxvm/images/imported/x/disk1.raw"}) {
			t.Fatalf("import = %+v, data disks %+v", got, disks)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("FluxVM import called %d times, want 1", calls.Load())
	}
	name := importName(m.Spec.Image)
	if !strings.HasSuffix(name, "-repaired") || !strings.Contains(gotBody, `"name":"`+name+`"`) || !strings.Contains(gotBody, `"source":"/cache/sha256/abc"`) || !strings.Contains(gotBody, `"repair":true`) {
		t.Fatalf("name %q body %s", name, gotBody)
	}
	if _, err := os.Stat(filepath.Join(a.ImageCacheDir, "imported", name+".json")); err != nil {
		t.Fatal(err)
	}
}

// A qcow2 whose header names a backing file must not be used as a boot image:
// qemu-img/the hypervisor would read that node-local path into the guest.
func TestResolveImageSourceRejectsBackedQcow2(t *testing.T) {
	hdr := make([]byte, 72)
	copy(hdr, qcow2Magic)
	hdr[8+7] = 0x48 // backing_file_offset != 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(hdr)
	}))
	defer srv.Close()
	a := &Agent{ImageCacheDir: t.TempDir()}
	_, err := a.resolveImageSource(context.Background(), machineWithImageSource(srv.URL, digestOf(hdr)))
	if err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("want backing-file rejection, got %v", err)
	}
	plain := append([]byte(nil), hdr...)
	plain[8+7] = 0
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), plain, 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(p, plain, 0o600)
	if err := rejectBackedQcow2(p); err != nil {
		t.Fatalf("standalone qcow2 rejected: %v", err)
	}
}
