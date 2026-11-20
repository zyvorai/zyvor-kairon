// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// ImageDigestPrefix is the only digest algorithm spec.image.digest
// supports for a spec.image.source Machine.
const ImageDigestPrefix = "sha256:"

// OCIReference is a parsed spec.image.source.oci registry reference.
type OCIReference struct {
	// Registry is the host[:port] the v2 API is served from.
	Registry string
	// Repository is the path below /v2/, e.g. library/fedora.
	Repository string
	Tag        string
	Digest     string
}

// ParseOCIReference parses [registry/]repository[:tag][@sha256:hex].
// A reference without a registry host means Docker Hub, with the
// library/ prefix for single-component names, as docker pull does.
func ParseOCIReference(ref string) (OCIReference, error) {
	var r OCIReference
	rest := strings.TrimSpace(ref)
	if rest == "" || strings.ContainsAny(rest, " \t\n") || strings.Contains(rest, "://") {
		return r, fmt.Errorf("invalid OCI reference %q (want registry/repository[:tag][@sha256:...], no scheme)", ref)
	}
	if name, digest, ok := strings.Cut(rest, "@"); ok {
		if !validSHA256Digest(digest) {
			return r, fmt.Errorf("OCI reference %q: digest must be sha256: plus 64 hex characters", ref)
		}
		r.Digest, rest = digest, name
	}
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		r.Tag, rest = rest[i+1:], rest[:i]
		if r.Tag == "" {
			return r, fmt.Errorf("OCI reference %q: empty tag", ref)
		}
	}
	first, remainder, hasSlash := strings.Cut(rest, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repository = first, remainder
	} else {
		r.Registry, r.Repository = "registry-1.docker.io", rest
		if !hasSlash {
			r.Repository = "library/" + rest
		}
	}
	if r.Repository == "" || r.Repository != strings.ToLower(r.Repository) {
		return r, fmt.Errorf("OCI reference %q: repository must be non-empty and lowercase", ref)
	}
	for _, part := range strings.Split(r.Repository, "/") {
		if part == "" || part == "." || part == ".." {
			return r, fmt.Errorf("OCI reference %q: invalid repository path", ref)
		}
	}
	return r, nil
}

func validSHA256Digest(d string) bool {
	h, ok := strings.CutPrefix(d, ImageDigestPrefix)
	if !ok || len(h) != 64 || h != strings.ToLower(h) {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// Location names the source for logs and errors.
func (s ImageSource) Location() string {
	if s.OCI != "" {
		return "oci://" + s.OCI
	}
	return s.HTTPURL
}

// ValidateImageSource enforces that exactly one of httpURL/oci is set and
// that spec.image.digest is a sha256 digest whenever source is: it is the
// node cache key and, for oci, the pinned manifest (or index) digest.
// Shared by kairon-node and the admission webhook.
func ValidateImageSource(img ImageSpec) error {
	if img.Source == nil {
		return nil
	}
	httpURL, oci := strings.TrimSpace(img.Source.HTTPURL), strings.TrimSpace(img.Source.OCI)
	switch {
	case httpURL == "" && oci == "":
		return fmt.Errorf("spec.image.source needs httpURL or oci")
	case httpURL != "" && oci != "":
		return fmt.Errorf("spec.image.source.httpURL and spec.image.source.oci are mutually exclusive")
	case httpURL != "":
		if !strings.HasPrefix(httpURL, "http://") && !strings.HasPrefix(httpURL, "https://") {
			return fmt.Errorf("spec.image.source.httpURL %q must be an http:// or https:// URL", httpURL)
		}
	}
	if !slices.Contains(ImageSourceFormats, img.Source.Format) {
		return fmt.Errorf("spec.image.source.format %q must be one of qcow2, raw, ova, vmdk, vhd, vhdx", img.Source.Format)
	}
	if !validSHA256Digest(img.Digest) {
		return fmt.Errorf("spec.image.digest must be set as %q plus a 64-character hex digest when spec.image.source is set (it's the cache key -- see docs/guides/machine-image-import.md)", ImageDigestPrefix)
	}
	if oci != "" {
		ref, err := ParseOCIReference(oci)
		if err != nil {
			return fmt.Errorf("spec.image.source.oci: %w", err)
		}
		if ref.Digest != "" && ref.Digest != img.Digest {
			return fmt.Errorf("spec.image.source.oci digest %s does not match spec.image.digest %s", ref.Digest, img.Digest)
		}
	}
	return nil
}
