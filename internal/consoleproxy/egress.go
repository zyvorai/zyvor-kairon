// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type egressCheckRequest struct {
	Host string `json:"host"`
}

// handleEgressCheck forwards to FluxVM's own POST /v1/egress/check --
// node-scoped (not per-VM), a stateless diagnostic against the node's own
// static [sandbox] egress config. Relayed verbatim, including whatever
// FluxVM's own inject_authorization field carries -- a real credential
// vault secret when non-empty. internal/uiapi.handleEgressCheck, not this
// hop, is responsible for redacting it before it ever reaches an external
// caller; kairon-node is an already-trusted internal relay, kairon-ui is
// the actual external-facing boundary.
func (s *Server) handleEgressCheck(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req egressCheckRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	decision, err := s.Flux.EgressCheck(r.Context(), req.Host)
	if err != nil {
		http.Error(w, fmt.Sprintf("egress check: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(decision)
}

// maxJSONBody caps small control-plane JSON request bodies so a caller
// holding the shared token cannot make the node buffer unbounded input.
const maxJSONBody = 1 << 20

// decodeJSONBody decodes a size-limited JSON request body into v.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(v)
}
