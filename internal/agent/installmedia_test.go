// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestBlankBaseIsSharedAndSized(t *testing.T) {
	a := &Agent{ImageCacheDir: t.TempDir()}
	p1, err := a.blankBase()
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := a.blankBase()
	fi, err := os.Stat(p1)
	if err != nil || p1 != p2 || fi.Size() != blankBaseSize {
		t.Fatalf("p1=%s p2=%s fi=%v err=%v", p1, p2, fi, err)
	}
	if _, err := (&Agent{}).blankBase(); err == nil {
		t.Fatal("no cache dir must fail")
	}
}

func TestPopulateBootVolume(t *testing.T) {
	dir := t.TempDir()
	blank := filepath.Join(dir, "blank", "disk.img")
	_ = os.MkdirAll(filepath.Dir(blank), 0o755)
	if err := populateBootVolume(blank, "", 2, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(blank); fi.Size() != 2<<30 {
		t.Fatalf("blank size %d", fi.Size())
	}
	if err := populateBootVolume(filepath.Join(dir, "x.img"), "", 0, false); err == nil {
		t.Fatal("blank without size must fail")
	}

	src := filepath.Join(dir, "golden.qcow2")
	_ = os.WriteFile(src, []byte("golden"), 0o644)
	seeded := filepath.Join(dir, "disk.img")
	if err := populateBootVolume(seeded, src, 0, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(seeded); string(b) != "golden" {
		t.Fatalf("seeded %q", b)
	}
	_ = os.WriteFile(src, []byte("republished"), 0o644)
	if err := populateBootVolume(seeded, src, 0, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(seeded); string(b) != "golden" {
		t.Fatal("an existing boot disk must never be overwritten")
	}
}

func TestPopulateSharedBootVolumeIsRaw(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "qemu-img")
	// The last argument is the output file.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do out=$a; done\nprintf converted > \"$out\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := qemuImg
	qemuImg = fake
	t.Cleanup(func() { qemuImg = orig })

	raw := filepath.Join(dir, "golden.raw")
	_ = os.WriteFile(raw, []byte("rawbytes"), 0o644)
	fromRaw := filepath.Join(dir, "a", "disk.img")
	_ = os.MkdirAll(filepath.Dir(fromRaw), 0o755)
	if err := populateBootVolume(fromRaw, raw, 0, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fromRaw); string(b) != "rawbytes" {
		t.Fatalf("a raw image must be copied as is, got %q", b)
	}

	qcow := filepath.Join(dir, "golden.qcow2")
	_ = os.WriteFile(qcow, append([]byte("QFI\xfb"), make([]byte, 60)...), 0o644)
	fromQcow := filepath.Join(dir, "b", "disk.img")
	_ = os.MkdirAll(filepath.Dir(fromQcow), 0o755)
	if err := populateBootVolume(fromQcow, qcow, 0, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(fromQcow); string(b) != "converted" {
		t.Fatalf("a qcow2 image must go through qemu-img convert, got %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "b", ".seed-*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	qemuImg = filepath.Join(dir, "missing-qemu-img")
	if err := populateBootVolume(filepath.Join(dir, "c.img"), qcow, 0, true); err == nil || !strings.Contains(err.Error(), "qemu-img") {
		t.Fatalf("a qcow2 image without qemu-img must fail clearly, got %v", err)
	}
}

func TestResolveCdromsDownloadsIntoCache(t *testing.T) {
	iso := []byte("ISO9660 bytes")
	sum := sha256.Sum256(iso)
	digest := model.ImageDigestPrefix + hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(iso) }))
	defer srv.Close()

	a := &Agent{ImageCacheDir: t.TempDir()}
	m := model.Machine{Spec: model.MachineSpec{Cdroms: []model.MachineCdrom{
		{Name: "install", Source: &model.ImageSource{HTTPURL: srv.URL + "/win.iso"}, Digest: digest},
	}}}
	if err := a.resolveCdroms(context.Background(), &m); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("self-signed server without opt-in must fail TLS verification, got %v", err)
	}
	m.Spec.Cdroms[0].Source.InsecureSkipTLSVerify = true
	if err := a.resolveCdroms(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(m.Spec.Cdroms[0].Path); string(b) != string(iso) {
		t.Fatalf("cached %q at %s", b, m.Spec.Cdroms[0].Path)
	}

	m.Spec.Cdroms[0].Digest = model.ImageDigestPrefix + strings.Repeat("0", 64)
	m.Spec.Cdroms[0].Path = ""
	if err := a.resolveCdroms(context.Background(), &m); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("insecure TLS must still verify the digest, got %v", err)
	}

	unresolved := model.Machine{Spec: model.MachineSpec{Cdroms: []model.MachineCdrom{{Name: "install", ImageRef: "win-iso"}}}}
	if err := a.resolveCdroms(context.Background(), &unresolved); err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("unresolved imageRef: %v", err)
	}
}
