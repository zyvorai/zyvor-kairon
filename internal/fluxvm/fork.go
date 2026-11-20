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

// Fork starts count children of running VM id from one memory snapshot
// (POST /v1/vms/{id}/fork). Children are named "{namePrefix}-{n}", n from
// 1. FluxVM only forks flux-vm backend VMs whose network is none, user, or
// a per-VM-netns tap, and creates either every child or none.
func (c *Client) Fork(ctx context.Context, id string, count int, namePrefix string) ([]Record, error) {
	body := map[string]any{"count": count}
	if namePrefix != "" {
		body["namePrefix"] = namePrefix
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/fork", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Record `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode fork response: %w", err)
	}
	if len(out.Items) != count {
		return nil, fmt.Errorf("fork returned %d children, want %d", len(out.Items), count)
	}
	return out.Items, nil
}
