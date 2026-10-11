// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// BalloonSpec is FluxVM's CreateVmRequest.balloon (QEMU only).
type BalloonSpec struct {
	FreePageReporting bool `json:"free_page_reporting"`
	DeflateOnOOM      bool `json:"deflate_on_oom"`
}

// BalloonStatus is FluxVM's GET/POST /v1/vms/{id}/balloon. TargetMiB and
// ActualMiB are memory taken away from the guest.
type BalloonStatus struct {
	MemoryMiB uint64 `json:"memory_mib"`
	TargetMiB uint64 `json:"target_mib"`
	ActualMiB uint64 `json:"actual_mib"`
}

// GuestMemoryStats is what the guest's balloon driver reports, in MiB.
type GuestMemoryStats struct {
	TotalMiB     *uint64 `json:"total_mib"`
	FreeMiB      *uint64 `json:"free_mib"`
	AvailableMiB *uint64 `json:"available_mib"`
	LastUpdate   uint64  `json:"last_update"`
}

// VMMemoryReport is FluxVM's GET /v1/vms/{id}/memory.
type VMMemoryReport struct {
	ConfiguredMiB uint64            `json:"configured_mib"`
	Balloon       *BalloonStatus    `json:"balloon"`
	Guest         *GuestMemoryStats `json:"guest,omitempty"`
}

// HostMemory is FluxVM's GET /v1/host/memory. A field is nil when the
// host does not expose it.
type HostMemory struct {
	TotalMiB     *uint64  `json:"mem_total_mib"`
	AvailableMiB *uint64  `json:"mem_available_mib"`
	PSISomeAvg10 *float64 `json:"psi_some_avg10"`
	PSIFullAvg10 *float64 `json:"psi_full_avg10"`
}

// GetHostMemory samples host memory and memory PSI.
func (c *Client) GetHostMemory(ctx context.Context) (*HostMemory, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/host/memory", nil)
	if err != nil {
		return nil, err
	}
	var out HostMemory
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM host memory: %w", err)
	}
	return &out, nil
}

// SetBalloon asks the guest to give back balloonMiB (0 deflates).
func (c *Client) SetBalloon(ctx context.Context, id string, balloonMiB uint64) (*BalloonStatus, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/balloon", map[string]any{"balloon_mib": balloonMiB})
	if err != nil {
		return nil, err
	}
	var out BalloonStatus
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM balloon status: %w", err)
	}
	return &out, nil
}

// GetMemoryReport returns the VM's balloon state and guest statistics.
func (c *Client) GetMemoryReport(ctx context.Context, id string) (*VMMemoryReport, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/memory", nil)
	if err != nil {
		return nil, err
	}
	var out VMMemoryReport
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM memory report: %w", err)
	}
	return &out, nil
}
