// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

// handleCreatePool forwards to FluxVM's own POST /v1/pools.
func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var spec fluxvm.PoolSpec
	if err := decodeJSONBody(w, r, &spec); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	record, err := s.Flux.CreatePool(r.Context(), spec)
	if err != nil {
		http.Error(w, fmt.Sprintf("create pool: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(record)
}

// handleListPools forwards to FluxVM's own GET /v1/pools.
func (s *Server) handleListPools(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	items, err := s.Flux.ListPools(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("list pools: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// handleGetPool forwards to FluxVM's own GET /v1/pools/{name}.
func (s *Server) handleGetPool(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	record, err := s.Flux.GetPool(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, fmt.Sprintf("get pool: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(record)
}

// handleDeletePool forwards to FluxVM's own DELETE /v1/pools/{name} --
// destroys every member VM the pool currently holds, claimed or not.
func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	if err := s.Flux.DeletePool(r.Context(), r.PathValue("name")); err != nil {
		http.Error(w, fmt.Sprintf("delete pool: %v", err), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClaimPool forwards to FluxVM's own POST /v1/pools/{name}/claim.
func (s *Server) handleClaimPool(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var overrides fluxvm.ClaimOverrides
	if err := decodeJSONBody(w, r, &overrides); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	rec, err := s.Flux.ClaimFromPool(r.Context(), r.PathValue("name"), overrides)
	if err != nil {
		http.Error(w, fmt.Sprintf("claim from pool: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(rec)
}
