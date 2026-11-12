// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	maxManifestBytes    = 4 << 20
)

type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type ociDescriptor struct {
	MediaType string       `json:"mediaType"`
	Digest    string       `json:"digest"`
	Size      int64        `json:"size"`
	Platform  *ociPlatform `json:"platform,omitempty"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Manifests []ociDescriptor `json:"manifests"`
	Layers    []ociDescriptor `json:"layers"`
}

func (m ociManifest) isIndex() bool {
	return m.MediaType == mediaOCIIndex || m.MediaType == mediaDockerList || (m.MediaType == "" && len(m.Manifests) > 0)
}

// registryClient is a minimal pull-only OCI distribution (registry v2)
// client: HTTPS only, anonymous or anonymous-bearer-token auth.
type registryClient struct {
	http  *http.Client
	ref   model.OCIReference
	token string
	// maxBytes caps one layer blob (as served) and the disk file extracted
	// from it; 0 means DefaultMaxImageDownloadBytes.
	maxBytes int64
}

func (c *registryClient) limit() int64 {
	if c.maxBytes > 0 {
		return c.maxBytes
	}
	return DefaultMaxImageDownloadBytes
}

func (c *registryClient) get(ctx context.Context, p, accept string) (*http.Response, error) {
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
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			if err := c.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: unexpected status %s", u, resp.Status)
		}
		return resp, nil
	}
}

// authenticate follows a Bearer WWW-Authenticate challenge to fetch an
// anonymous pull token. Registries that demand credentials are rejected:
// kairon-node holds no registry secrets.
func (c *registryClient) authenticate(ctx context.Context, challenge string) error {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("registry %s requires %q authentication; only anonymous pulls are supported", c.ref.Registry, scheme)
	}
	params := parseAuthParams(rest)
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return fmt.Errorf("registry %s: token realm %q must be an https URL", c.ref.Registry, params["realm"])
	}
	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + c.ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry token from %s: %w", realm.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token from %s: unexpected status %s (private images are not supported)", realm.Host, resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return fmt.Errorf("registry token from %s: %w", realm.Host, err)
	}
	c.token = tok.Token
	if c.token == "" {
		c.token = tok.AccessToken
	}
	if c.token == "" {
		return fmt.Errorf("registry token from %s: empty token", realm.Host)
	}
	return nil
}

// parseAuthParams parses the comma-separated key="value" list of a
// WWW-Authenticate challenge; quoted values may contain commas.
func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; {
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				val, rest = rest[1:], ""
			} else {
				val, rest = rest[1:end+1], rest[end+2:]
			}
		} else {
			val, rest, _ = strings.Cut(rest, ",")
			rest = "," + rest
		}
		out[key] = val
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return out
}

// manifest fetches digest and verifies the bytes against it.
func (c *registryClient) manifest(ctx context.Context, digest string) (ociManifest, error) {
	resp, err := c.get(ctx, "/manifests/"+digest, strings.Join([]string{mediaOCIIndex, mediaOCIManifest, mediaDockerList, mediaDockerManifest}, ", "))
	if err != nil {
		return ociManifest{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return ociManifest{}, fmt.Errorf("read manifest %s: %w", digest, err)
	}
	if len(body) > maxManifestBytes {
		return ociManifest{}, fmt.Errorf("manifest %s is larger than %d bytes", digest, maxManifestBytes)
	}
	if got := sha256.Sum256(body); model.ImageDigestPrefix+hex.EncodeToString(got[:]) != digest {
		return ociManifest{}, fmt.Errorf("manifest digest mismatch: registry returned sha256:%x for %s", got, digest)
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ociManifest{}, fmt.Errorf("decode manifest %s: %w", digest, err)
	}
	if m.MediaType == "" {
		m.MediaType, _, _ = strings.Cut(resp.Header.Get("Content-Type"), ";")
	}
	return m, nil
}

// pickPlatform returns the linux/<arch> entry of an image index.
func pickPlatform(manifests []ociDescriptor, arch string) (ociDescriptor, error) {
	for _, d := range manifests {
		if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == arch {
			return d, nil
		}
	}
	return ociDescriptor{}, fmt.Errorf("image index has no linux/%s manifest", arch)
}

func (a *Agent) registryHTTP() *http.Client {
	if a.RegistryHTTP != nil {
		return a.RegistryHTTP
	}
	return http.DefaultClient
}

// pullOCIDisk pulls the containerDisk named by img (manifest or index
// digest img.Digest) and writes its disk/ file to destPath. Layers are
// searched top-down; each is streamed once and its digest checked before
// anything is renamed into place.
func (a *Agent) pullOCIDisk(ctx context.Context, img model.ImageSpec, destPath string) error {
	ref, err := model.ParseOCIReference(img.Source.OCI)
	if err != nil {
		return err
	}
	c := &registryClient{http: a.registryHTTP(), ref: ref, maxBytes: a.maxImageDownloadBytes()}
	m, err := c.manifest(ctx, img.Digest)
	if err != nil {
		return err
	}
	if m.isIndex() {
		d, err := pickPlatform(m.Manifests, runtime.GOARCH)
		if err != nil {
			return fmt.Errorf("%s: %w", img.Source.OCI, err)
		}
		if m, err = c.manifest(ctx, d.Digest); err != nil {
			return err
		}
		if m.isIndex() {
			return fmt.Errorf("%s: nested image index is not supported", img.Source.OCI)
		}
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("create image cache directory: %w", err)
	}
	for i := len(m.Layers) - 1; i >= 0; i-- {
		found, err := c.extractDisk(ctx, m.Layers[i], destPath)
		if err != nil {
			return fmt.Errorf("%s layer %s: %w", img.Source.OCI, m.Layers[i].Digest, err)
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("%s: no file under disk/ in any layer (containerDisk images keep the disk image in /disk/)", img.Source.OCI)
}

var errZstdLayer = errors.New("zstd-compressed layers are not supported; push the containerDisk with gzip or uncompressed layers")

func (c *registryClient) extractDisk(ctx context.Context, layer ociDescriptor, destPath string) (bool, error) {
	if !strings.HasPrefix(layer.Digest, model.ImageDigestPrefix) {
		return false, fmt.Errorf("unsupported layer digest %q", layer.Digest)
	}
	resp, err := c.get(ctx, "/blobs/"+layer.Digest, "")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	hasher := sha256.New()
	// One byte past the cap tells "exactly at the cap" from "endless body".
	blob := &io.LimitedReader{R: resp.Body, N: c.limit() + 1}
	raw := io.TeeReader(blob, hasher)
	br := bufio.NewReader(raw)
	magic, _ := br.Peek(4)
	var stream io.Reader = br
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return false, err
		}
		stream = gz
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return false, errZstdLayer
	}

	var tmpPath string
	defer func() {
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()
	tr := tar.NewReader(stream)
	for tmpPath == "" {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, fmt.Errorf("read layer tar: %w", err)
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if hdr.Typeflag != tar.TypeReg || path.Dir(name) != "disk" {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(destPath), ".oci-*")
		if err != nil {
			return false, err
		}
		tmpPath = tmp.Name()
		n, err := io.Copy(tmp, io.LimitReader(tr, c.limit()+1))
		if err == nil && n > c.limit() {
			err = fmt.Errorf("larger than the %d byte image size limit", c.limit())
		}
		if err != nil {
			_ = tmp.Close()
			return false, fmt.Errorf("extract %s: %w", name, err)
		}
		if err := tmp.Close(); err != nil {
			return false, err
		}
	}
	// Hash the rest of the blob so the digest covers every byte.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return false, fmt.Errorf("read layer: %w", err)
	}
	if blob.N <= 0 {
		return false, fmt.Errorf("layer is larger than the %d byte image size limit", c.limit())
	}
	if got := model.ImageDigestPrefix + hex.EncodeToString(hasher.Sum(nil)); got != layer.Digest {
		return false, fmt.Errorf("layer digest mismatch: got %s", got)
	}
	if tmpPath == "" {
		return false, nil
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return false, fmt.Errorf("finalize image cache entry: %w", err)
	}
	tmpPath = ""
	return true, nil
}
