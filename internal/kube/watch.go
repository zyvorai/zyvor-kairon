// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// WatchEvent is one Kubernetes watch notification. Object is the raw JSON
// of the watched resource; callers that need typed objects decode it.
type WatchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// WatchMachines opens a cluster-scoped Machines watch, optionally filtered
// by labelSelector. It streams events until ctx is cancelled or the server
// closes the connection. resourceVersion may be empty (start from now).
func (c *Client) WatchMachines(ctx context.Context, labelSelector, resourceVersion string, out chan<- WatchEvent) error {
	return c.watch(ctx, "/apis/kairon.zyvor.dev/v1/machines", labelSelector, resourceVersion, out)
}

// WatchMachineMigrations opens a cluster-scoped MachineMigrations watch.
func (c *Client) WatchMachineMigrations(ctx context.Context, labelSelector, resourceVersion string, out chan<- WatchEvent) error {
	return c.watch(ctx, "/apis/kairon.zyvor.dev/v1/machinemigrations", labelSelector, resourceVersion, out)
}

func (c *Client) watch(ctx context.Context, basePath, labelSelector, resourceVersion string, out chan<- WatchEvent) error {
	q := url.Values{}
	q.Set("watch", "true")
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	if resourceVersion != "" {
		q.Set("resourceVersion", resourceVersion)
	}
	path := basePath + "?" + q.Encode()

	// Watches are long-lived; do not use the short-timeout HTTP client.
	httpClient := c.HTTP
	if httpClient != nil && httpClient.Timeout > 0 {
		transport := httpClient.Transport
		httpClient = &http.Client{Transport: transport, Timeout: 0}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	start := time.Now()
	resp, err := httpClient.Do(req)
	if c.Observe != nil {
		// Only observe connection setup / immediate failure, not the full
		// watch lifetime (that would look like multi-minute "requests").
		if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.Observe(http.MethodGet, time.Since(start), err)
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode, Body: "watch rejected"}
	}

	scanner := bufio.NewScanner(resp.Body)
	// Watch objects can be large (full Machine JSON); raise the token size.
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev WatchEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("decode watch event: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- ev:
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}
