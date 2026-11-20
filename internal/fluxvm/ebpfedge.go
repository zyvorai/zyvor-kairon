// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

// ApplyEdge posts the compiled VM-edge document. FluxVM owns the BPF
// program; this only updates the maps it already pins under
// /sys/fs/bpf/fluxvm. POST /v1/vms/{id}/network/edge.
func (c *Client) ApplyEdge(ctx context.Context, id string, spec ebpfedge.EdgeSpec) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/edge", spec)
	return err
}

// ExportConntrack reads the per-Machine conntrack map.
// GET /v1/vms/{id}/network/conntrack.
func (c *Client) ExportConntrack(ctx context.Context, id string) (ebpfedge.ConntrackSnapshot, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/conntrack", nil)
	if err != nil {
		return ebpfedge.ConntrackSnapshot{}, err
	}
	var snap ebpfedge.ConntrackSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return ebpfedge.ConntrackSnapshot{}, err
	}
	return snap, nil
}

// RestoreConntrack writes a snapshot taken on the source before resume.
// POST /v1/vms/{id}/network/conntrack.
func (c *Client) RestoreConntrack(ctx context.Context, id string, snap ebpfedge.ConntrackSnapshot) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/conntrack", snap)
	return err
}

// AttributedDrops reads drop events that name the policy object which
// fired. GET /v1/vms/{id}/network/drops.
func (c *Client) AttributedDrops(ctx context.Context, id string, limit int) (json.RawMessage, error) {
	path := "/v1/vms/" + url.PathEscape(id) + "/network/drops"
	if limit > 0 {
		path += "?limit=" + itoa(limit)
	}
	return c.do(ctx, http.MethodGet, path, nil)
}

// StartCapture starts a bounded packet capture and returns FluxVM's view of
// the session (with its state). POST /v1/vms/{id}/network/capture.
func (c *Client) StartCapture(ctx context.Context, id string, session ebpfedge.CaptureSession) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/network/capture", session)
}

// CaptureSessions lists a VM's capture sessions with their state.
// GET /v1/vms/{id}/network/capture.
func (c *Client) CaptureSessions(ctx context.Context, id string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/capture", nil)
}

// CaptureFile opens a finished capture's pcap. The response is returned as
// is, error statuses included (409 while running, 404 when unknown), so a
// relay can pass them on; the caller closes the body.
// GET /v1/vms/{id}/network/capture/{token}.
func (c *Client) CaptureFile(ctx context.Context, id, token string) (*http.Response, error) {
	path := "/v1/vms/" + url.PathEscape(id) + "/network/capture/" + url.PathEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.HTTP.Do(req)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// LearnedIP reads an address the edge learned from ARP, DHCP, or ND.
// GET /v1/vms/{id}/network/learned-ip.
func (c *Client) LearnedIP(ctx context.Context, id string) (string, string, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/learned-ip", nil)
	if err != nil {
		return "", "", err
	}
	var body struct {
		IP     string `json:"ip"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", "", err
	}
	return body.IP, body.Source, nil
}
