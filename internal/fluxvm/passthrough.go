// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"net/url"
)

// Call forwards one request to FluxVM and returns the raw JSON body. The path
// is always a fixed FluxVM route chosen by the caller (never user-supplied
// text), with any identifier already path-escaped. A nil body sends none.
func (c *Client) Call(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	var payload any
	if len(body) > 0 {
		payload = json.RawMessage(body)
	}
	return c.do(ctx, method, path, payload)
}
