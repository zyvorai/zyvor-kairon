// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package tlsreload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedCert generates a fresh self-signed cert/key pair (a
// different CommonName each call, to distinguish "before" and "after" in
// reload tests) and writes them to certPath/keyPath.
func writeSelfSignedCert(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now,
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

func TestNewServesTheInitiallyLoadedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeSelfSignedCert(t, certPath, keyPath, "v1")

	w, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, keyPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cert, err := w.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected a loaded certificate")
	}
}

func TestNewFailsClosedOnBadCertPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("expected New to fail closed when the cert/key files don't exist")
	}
}

func TestGetClientCertificateReturnsTheSameMaterial(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeSelfSignedCert(t, certPath, keyPath, "v1")

	w, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, keyPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server, _ := w.GetCertificate(nil)
	client, _ := w.GetClientCertificate(nil)
	if !bytes.Equal(server.Certificate[0], client.Certificate[0]) {
		t.Fatal("GetCertificate and GetClientCertificate should serve the same loaded material")
	}
}

func TestRunReloadsOnFileChange(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeSelfSignedCert(t, certPath, keyPath, "v1")

	w, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, keyPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	original, _ := w.GetCertificate(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, 5*time.Millisecond)

	// Ensure a strictly later mtime than the original file, then write a
	// distinguishable new certificate over the same path.
	time.Sleep(10 * time.Millisecond)
	writeSelfSignedCert(t, certPath, keyPath, "v2")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, _ := w.GetCertificate(nil)
		if !bytes.Equal(current.Certificate[0], original.Certificate[0]) {
			return // reloaded successfully
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("certificate was not reloaded within the deadline")
}

func TestRunKeepsServingLastGoodCertificateOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeSelfSignedCert(t, certPath, keyPath, "v1")

	w, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), certPath, keyPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	original, _ := w.GetCertificate(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx, 5*time.Millisecond)

	time.Sleep(10 * time.Millisecond)
	// Corrupt the key file (newer mtime, but no longer a valid pair) --
	// the reload attempt should fail and the original certificate should
	// keep being served, not an error or a zero-value certificate.
	if err := os.WriteFile(keyPath, []byte("not a valid key"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	current, err := w.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if !bytes.Equal(current.Certificate[0], original.Certificate[0]) {
		t.Fatal("expected the last-good certificate to still be served after a failed reload")
	}
}
