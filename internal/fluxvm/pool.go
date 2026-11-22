// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// PoolSpec mirrors FluxVM's own PoolSpec (fluxvm-core's model module) --
// a warm-VM pool request: Size VMs pre-booted and kept Paused, ready for
// an instant claim. Template is a full CreateRequest, any backend.
type PoolSpec struct {
	Name     string        `json:"name"`
	Size     int           `json:"size"`
	Template CreateRequest `json:"template"`
}

// PoolRecord mirrors FluxVM's own PoolRecord -- persisted, per-node warm
// pool state. Members are VM UUIDs, each a real Paused Record in the same
// VM store every other route in this client talks to.
type PoolRecord struct {
	Name     string        `json:"name"`
	Size     int           `json:"size"`
	Template CreateRequest `json:"template"`
	Members  []string      `json:"members"`
}

// ClaimOverrides mirrors FluxVM's own ClaimOverrides -- applied to the VM
// handed back by ClaimFromPool, replacing whatever the pool's own
// template said for these two fields.
type ClaimOverrides struct {
	Name       string `json:"name,omitempty"`
	TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
}

// CreatePool creates a new warm-VM pool on this node -- FluxVM's own
// POST /v1/pools. Pools, like sandbox templates and the image catalog,
// are per-node state; FluxVM itself also fails outright if a pool with
// this name already exists on the node.
func (c *Client) CreatePool(ctx context.Context, spec PoolSpec) (*PoolRecord, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/pools", spec)
	if err != nil {
		return nil, err
	}
	var out PoolRecord
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM create pool: %w", err)
	}
	return &out, nil
}

// ListPools returns every warm-VM pool on this node -- FluxVM's own
// GET /v1/pools.
func (c *Client) ListPools(ctx context.Context) ([]PoolRecord, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/pools", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []PoolRecord `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM list pools: %w", err)
	}
	return out.Items, nil
}

// GetPool returns one pool's current state -- FluxVM's own
// GET /v1/pools/{name}.
func (c *Client) GetPool(ctx context.Context, name string) (*PoolRecord, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/pools/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var out PoolRecord
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM get pool: %w", err)
	}
	return &out, nil
}

// DeletePool deletes a pool and every one of its member VMs -- FluxVM's
// own DELETE /v1/pools/{name}. Unlike Delete (one VM), this is
// destructive to every VM the pool currently holds, claimed or not.
func (c *Client) DeletePool(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/pools/"+url.PathEscape(name), nil)
	return err
}

// ClaimFromPool pops one ready (pre-booted, Paused) member off a pool,
// resumes it, applies overrides, and returns the now-Running VM --
// FluxVM's own POST /v1/pools/{name}/claim. FluxVM triggers its own
// background backfill to replace the claimed member; this call does not
// wait for that. Fails with FluxVM's own clear error if the pool has no
// ready members right now, rather than silently falling back to a slow
// synchronous create.
func (c *Client) ClaimFromPool(ctx context.Context, name string, overrides ClaimOverrides) (*Record, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/pools/"+url.PathEscape(name)+"/claim", overrides)
	if err != nil {
		return nil, err
	}
	var out Record
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM claim from pool: %w", err)
	}
	return &out, nil
}
