// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// CatalogEntry mirrors FluxVM's own CatalogEntry exactly
// (fluxvm-image/src/catalog.rs) -- an alias a Machine's spec.image.catalogName
// (or FluxVM's own CreateVmRequest.image) can reference instead of a raw
// disk path. SHA256 is mandatory on FluxVM's own side: a catalog exists
// specifically to pin known-good images. Signature is only checked when
// FluxVM's own config.catalog.trusted_signers is non-empty -- Kairon
// neither generates nor verifies signatures itself, it only relays
// whatever FluxVM reports.
type CatalogEntry struct {
	Name      string `json:"name"`
	Source    string `json:"source"`
	SHA256    string `json:"sha256"`
	Format    string `json:"format,omitempty"`
	Distro    string `json:"distro,omitempty"`
	Version   string `json:"version,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Signature string `json:"signature,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// CatalogListEntry mirrors FluxVM's own CatalogListEntry -- CatalogEntry's
// own fields plus whether its signature actually verified, embedded
// (Go's own equivalent of Rust's #[serde(flatten)]) rather than nested
// under a separate key.
type CatalogListEntry struct {
	CatalogEntry
	// SignatureValid is nil when FluxVM's own trusted_signers list is
	// empty (signatures aren't required, so this is meaningless);
	// otherwise whether the entry's signature verified against at least
	// one configured trusted signer.
	SignatureValid *bool `json:"signature_valid"`
}

// ListCatalog returns every entry in this node's image catalog -- FluxVM's
// own GET /v1/images/catalog. Per-node state, like sandbox templates: an
// entry registered on one node is invisible from another.
func (c *Client) ListCatalog(ctx context.Context) ([]CatalogListEntry, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/images/catalog", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []CatalogListEntry `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM list catalog: %w", err)
	}
	return out.Items, nil
}

// AddCatalogEntry registers a new catalog entry -- FluxVM's own
// POST /v1/images/catalog. source is a local path or http(s):// URL;
// FluxVM downloads/verifies it and computes its own SHA-256 (this route
// has no way to pre-supply a digest, unlike Kairon's own
// spec.image.source image-import cache, which requires one up front).
// format defaults to "qcow2" server-side when empty.
func (c *Client) AddCatalogEntry(ctx context.Context, name, source, format string) (*CatalogEntry, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/images/catalog", map[string]any{"name": name, "source": source, "format": format})
	if err != nil {
		return nil, err
	}
	var out CatalogEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM add catalog entry: %w", err)
	}
	return &out, nil
}

// RemoveCatalogEntry deletes a catalog entry -- FluxVM's own
// DELETE /v1/images/catalog/{name}. Refused by FluxVM itself if the
// entry is marked read_only.
func (c *Client) RemoveCatalogEntry(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/images/catalog/"+url.PathEscape(name), nil)
	return err
}

// RenameCatalogEntry renames a catalog entry in place -- FluxVM's own
// POST /v1/images/catalog/{name}/rename.
func (c *Client) RenameCatalogEntry(ctx context.Context, name, newName string) (*CatalogEntry, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/images/catalog/"+url.PathEscape(name)+"/rename", map[string]any{"new_name": newName})
	if err != nil {
		return nil, err
	}
	var out CatalogEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM rename catalog entry: %w", err)
	}
	return &out, nil
}

// CloneCatalogEntry copies an existing catalog entry's underlying image
// under a new name -- FluxVM's own POST /v1/images/catalog/{name}/clone.
// Useful for branching off a read_only base image without touching it.
func (c *Client) CloneCatalogEntry(ctx context.Context, name, targetName string) (*CatalogEntry, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/images/catalog/"+url.PathEscape(name)+"/clone", map[string]any{"target_name": targetName})
	if err != nil {
		return nil, err
	}
	var out CatalogEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM clone catalog entry: %w", err)
	}
	return &out, nil
}

// ExportCatalogEntry copies a catalog entry's underlying image file out to
// an explicit node-local path -- FluxVM's own
// POST /v1/images/catalog/{name}/export.
func (c *Client) ExportCatalogEntry(ctx context.Context, name, path string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/images/catalog/"+url.PathEscape(name)+"/export", map[string]any{"path": path})
	return err
}

// SetCatalogReadOnly toggles a catalog entry's read_only flag -- FluxVM's
// own POST /v1/images/catalog/{name}/read-only. A read_only entry refuses
// RemoveCatalogEntry/RenameCatalogEntry on FluxVM's own side -- protects a
// base image other entries are cloned from.
func (c *Client) SetCatalogReadOnly(ctx context.Context, name string, readOnly bool) (*CatalogEntry, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/images/catalog/"+url.PathEscape(name)+"/read-only", map[string]any{"read_only": readOnly})
	if err != nil {
		return nil, err
	}
	var out CatalogEntry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM set catalog read-only: %w", err)
	}
	return &out, nil
}

// CleanCatalogDownloads removes orphaned download artifacts left behind
// by AddCatalogEntry's own image fetch -- FluxVM's own
// POST /v1/images/catalog/clean. Returns how many files were removed.
func (c *Client) CleanCatalogDownloads(ctx context.Context) (int, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/images/catalog/clean", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Removed int `json:"removed"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("decode FluxVM clean catalog: %w", err)
	}
	return out.Removed, nil
}
