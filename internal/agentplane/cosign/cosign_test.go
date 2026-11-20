// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package cosign

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const imageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func sign(t *testing.T, key *ecdsa.PrivateKey, digest string) ([]byte, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"critical": map[string]any{
		"identity": map[string]any{"docker-reference": "registry/agents/runner"},
		"image":    map[string]any{"docker-manifest-digest": digest},
		"type":     "cosign container image signature",
	}})
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return payload, base64.StdEncoding.EncodeToString(sig)
}

// fakeRegistry serves one signature manifest for imageDigest.
func fakeRegistry(t *testing.T, payload []byte, sig string, hits *atomic.Int32) *httptest.Server {
	sum := sha256.Sum256(payload)
	blob := "sha256:" + hex.EncodeToString(sum[:])
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"layers": []map[string]any{{
			"mediaType":   mediaSimpleSigning,
			"digest":      blob,
			"size":        len(payload),
			"annotations": map[string]string{annSignature: sig},
		}},
	})
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/v2/agents/runner/manifests/sha256-" + strings.TrimPrefix(imageDigest, "sha256:") + ".sig":
			_, _ = w.Write(manifest)
		case "/v2/agents/runner/blobs/" + blob:
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
}

func pubPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestVerifyAcceptsSignedDigestAndCaches(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	payload, sig := sign(t, key, imageDigest)
	var hits atomic.Int32
	srv := fakeRegistry(t, payload, sig, &hits)
	defer srv.Close()
	pub, err := LoadPublicKey(pubPEM(t, key))
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Key: pub, HTTP: srv.Client()}
	ref := strings.TrimPrefix(srv.URL, "https://") + "/agents/runner:v1"
	if err := v.Verify(context.Background(), ref, imageDigest); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if err := v.Verify(context.Background(), ref, imageDigest); err != nil || hits.Load() != before {
		t.Fatalf("second verify should be cached: err=%v hits %d -> %d", err, before, hits.Load())
	}
}

func TestVerifyRejectsWrongKeyAndMissingSignature(t *testing.T) {
	signer, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	payload, sig := sign(t, signer, imageDigest)
	var hits atomic.Int32
	srv := fakeRegistry(t, payload, sig, &hits)
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "https://") + "/agents/runner"

	v := &Verifier{Key: &other.PublicKey, HTTP: srv.Client()}
	if err := v.Verify(context.Background(), ref, imageDigest); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("wrong key: %v", err)
	}
	v = &Verifier{Key: &signer.PublicKey, HTTP: srv.Client()}
	unsigned := "sha256:" + strings.Repeat("2", 64)
	if err := v.Verify(context.Background(), ref, unsigned); err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("unsigned digest: %v", err)
	}
}

func TestVerifyPayloadRejectsOtherDigest(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	payload, sig := sign(t, key, "sha256:"+strings.Repeat("3", 64))
	if err := VerifyPayload(&key.PublicKey, payload, sig, imageDigest); err == nil || !strings.Contains(err.Error(), "not "+imageDigest) {
		t.Fatalf("got %v", err)
	}
	if _, err := LoadPublicKey([]byte("nope")); err == nil {
		t.Fatal("garbage key accepted")
	}
}
