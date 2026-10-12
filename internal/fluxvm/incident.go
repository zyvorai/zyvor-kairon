// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DiagnosticsRequest is the body of FluxVM's POST /v1/vms/{id}/diagnostics.
type DiagnosticsRequest struct {
	PcapSeconds int    `json:"pcap_seconds,omitempty"`
	PcapFilter  string `json:"pcap_filter,omitempty"`
	MemoryDump  bool   `json:"memory_dump,omitempty"`
	Redact      bool   `json:"redact"`
	LogLines    int    `json:"log_lines,omitempty"`
}

// DiagnosticsSection is one part of a bundle and whether it was collected.
type DiagnosticsSection struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Bytes int64  `json:"bytes"`
}

// MemoryDump is a guest memory dump FluxVM keeps on its host.
type MemoryDump struct {
	Path      string `json:"path"`
	Format    string `json:"format"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	PausedMs  int64  `json:"paused_ms"`
}

// DiagnosticsBundle is a FluxVM incident bundle job; State is running,
// done or failed.
type DiagnosticsBundle struct {
	Token      string               `json:"token"`
	VMID       string               `json:"vm_id"`
	State      string               `json:"state"`
	CreatedAt  time.Time            `json:"created_at"`
	FinishedAt *time.Time           `json:"finished_at,omitempty"`
	Request    DiagnosticsRequest   `json:"request"`
	Error      string               `json:"error,omitempty"`
	Sections   []DiagnosticsSection `json:"sections,omitempty"`
	SizeBytes  int64                `json:"size_bytes"`
	SHA256     string               `json:"sha256,omitempty"`
	MemoryDump *MemoryDump          `json:"memory_dump,omitempty"`
}

// Diagnostics calls /v1/vms/{id}/diagnostics + suffix and returns the
// response as is, error statuses included, so a relay can pass FluxVM's
// status and message on. Downloads are bounded only by ctx: a memory dump
// can be as large as the guest. The caller closes the body.
func (c *Client) Diagnostics(ctx context.Context, method, id, suffix string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/v1/vms/"+url.PathEscape(id)+"/diagnostics"+suffix, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := &http.Client{}
	if c.HTTP != nil {
		hc.Transport = c.HTTP.Transport
	}
	return hc.Do(req)
}
