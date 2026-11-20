// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package cosign verifies key-based cosign signatures of OCI images.
// It reads the legacy sha256-<hex>.sig tag from the image's own
// repository, checks each simple-signing layer's signature against a
// configured public key, and requires the signed payload to name the
// exact manifest digest. Keyless (Fulcio and Rekor) signatures are not
// verified here.
package cosign

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	mediaSimpleSigning = "application/vnd.dev.cosign.simplesigning.v1+json"
	annSignature       = "dev.cosignproject.cosign/signature"
	maxManifestBytes   = 4 << 20
	maxPayloadBytes    = 1 << 20
	cacheTTL           = 10 * time.Minute
)

// LoadPublicKey parses a PEM PKIX public key (ECDSA, Ed25519 or RSA), the
// format `cosign generate-key-pair` writes to cosign.pub.
func LoadPublicKey(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("cosign public key: no PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("cosign public key: %w", err)
	}
	switch key.(type) {
	case *ecdsa.PublicKey, ed25519.PublicKey, *rsa.PublicKey:
		return key, nil
	}
	return nil, fmt.Errorf("cosign public key: unsupported type %T", key)
}

// Verifier checks signatures for one public key. Successful results are
// cached per repository and digest for ten minutes.
type Verifier struct {
	Key  crypto.PublicKey
	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]time.Time
}

// Verify returns nil when ref's repository holds a signature by v.Key for
// digest. ref is [registry/]repository[:tag][@sha256:...]; digest is the
// manifest digest the Machine boots.
func (v *Verifier) Verify(ctx context.Context, ref, digest string) error {
	if v == nil || v.Key == nil {
		return errors.New("cosign: no public key configured")
	}
	r, err := model.ParseOCIReference(ref)
	if err != nil {
		return err
	}
	hexPart, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexPart) != 64 {
		return fmt.Errorf("cosign: digest must be sha256:<64 hex>")
	}
	key := r.Registry + "/" + r.Repository + "@" + digest
	if v.cached(key) {
		return nil
	}
	c := &registry{http: v.client(), ref: r}
	body, err := c.fetch(ctx, "/manifests/sha256-"+hexPart+".sig", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json", maxManifestBytes)
	if err != nil {
		return fmt.Errorf("cosign: no signature for %s: %w", digest, err)
	}
	var man struct {
		Layers []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &man); err != nil {
		return fmt.Errorf("cosign: signature manifest: %w", err)
	}
	lastErr := errors.New("signature manifest has no simple-signing layer")
	for _, l := range man.Layers {
		if l.MediaType != mediaSimpleSigning || l.Annotations[annSignature] == "" {
			continue
		}
		payload, err := c.fetch(ctx, "/blobs/"+l.Digest, "", maxPayloadBytes)
		if err != nil {
			lastErr = err
			continue
		}
		if sum := sha256.Sum256(payload); "sha256:"+hex.EncodeToString(sum[:]) != l.Digest {
			lastErr = fmt.Errorf("payload blob does not match %s", l.Digest)
			continue
		}
		if err := VerifyPayload(v.Key, payload, l.Annotations[annSignature], digest); err != nil {
			lastErr = err
			continue
		}
		v.remember(key)
		return nil
	}
	return fmt.Errorf("cosign: %s: %w", digest, lastErr)
}

// VerifyPayload checks one simple-signing payload: the base64 signature
// must verify under key, and the payload must name digest.
func VerifyPayload(key crypto.PublicKey, payload []byte, sigB64, digest string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("signature is not base64: %w", err)
	}
	sum := sha256.Sum256(payload)
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, sum[:], sig) {
			return errors.New("signature does not verify")
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(k, payload, sig) {
			return errors.New("signature does not verify")
		}
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil && rsa.VerifyPSS(k, crypto.SHA256, sum[:], sig, nil) != nil {
			return errors.New("signature does not verify")
		}
	default:
		return fmt.Errorf("unsupported key type %T", key)
	}
	var p struct {
		Critical struct {
			Image struct {
				Digest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if p.Critical.Image.Digest != digest {
		return fmt.Errorf("payload signs %s, not %s", p.Critical.Image.Digest, digest)
	}
	return nil
}

func (v *Verifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (v *Verifier) cached(key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.cache[key]
	return ok && time.Since(t) < cacheTTL
}

func (v *Verifier) remember(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cache == nil {
		v.cache = map[string]time.Time{}
	}
	v.cache[key] = time.Now()
}

// registry is a pull-only OCI distribution client with anonymous bearer
// token support, the same trust model as kairon-node's containerDisk pull.
type registry struct {
	http  *http.Client
	ref   model.OCIReference
	token string
}

func (c *registry) fetch(ctx context.Context, p, accept string, limit int64) ([]byte, error) {
	u := "https://" + c.ref.Registry + "/v2/" + c.ref.Repository + p
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			if err := c.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > limit {
			return nil, fmt.Errorf("GET %s: response larger than %d bytes", u, limit)
		}
		return body, nil
	}
}

func (c *registry) authenticate(ctx context.Context, challenge string) error {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("registry %s requires %q authentication; only anonymous pulls are supported", c.ref.Registry, scheme)
	}
	params := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			params[strings.ToLower(k)] = strings.Trim(val, `"`)
		}
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return fmt.Errorf("registry %s: token realm %q must be an https URL", c.ref.Registry, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+c.ref.Repository+":pull")
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return err
	}
	c.token = tok.Token
	if c.token == "" {
		c.token = tok.AccessToken
	}
	if c.token == "" {
		return errors.New("registry token: empty")
	}
	return nil
}
