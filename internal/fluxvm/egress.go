// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// EgressDecision mirrors FluxVM's own EgressDecision (fluxvm-network's
// egress module) -- a stateless allow/deny check against the node's own
// static [sandbox] config (egress_allow_domains/credential_vault), not a
// runtime-mutable resource: there is no way to change the allowlist via
// this or any other FluxVM API, only by editing the node's own config and
// restarting.
//
// InjectAuthorization, when non-empty, is the literal credential-vault
// secret value FluxVM would inject for this host -- a real secret, not a
// boolean. internal/uiapi.handleEgressCheck deliberately never returns
// this field's actual value to a caller; see its own doc comment.
type EgressDecision struct {
	Allow               bool   `json:"allow"`
	InjectAuthorization string `json:"inject_authorization"`
	Reason              string `json:"reason"`
}

// EgressCheck asks FluxVM whether an outbound request from a sandbox to
// host would be allowed -- FluxVM's own POST /v1/egress/check. No auth
// requirement on FluxVM's own side (it's a read-only diagnostic against
// static config), but see internal/uiapi.handleEgressCheck for why
// Kairon still gates and redacts this.
func (c *Client) EgressCheck(ctx context.Context, host string) (*EgressDecision, error) {
	data, err := c.do(ctx, http.MethodPost, "/v1/egress/check", map[string]any{"host": host})
	if err != nil {
		return nil, err
	}
	var out EgressDecision
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM egress check: %w", err)
	}
	return &out, nil
}
