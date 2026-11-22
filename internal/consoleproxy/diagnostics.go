// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package consoleproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleRuntimeCapabilities forwards to FluxVM's own
// GET /v1/runtime/capabilities -- node-scoped, not per-VM.
func (s *Server) handleRuntimeCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	caps, err := s.Flux.GetRuntimeCapabilities(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("runtime capabilities: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(caps)
}

// handlePressure forwards to FluxVM's own GET /v1/vms/{id}/pressure.
func (s *Server) handlePressure(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	pressure, err := s.Flux.GetPressure(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("get pressure: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(pressure)
}

// handleCPUSet forwards to FluxVM's own GET /v1/vms/{id}/cpuset.
func (s *Server) handleCPUSet(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	cpus, err := s.Flux.GetCPUSet(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("get cpuset: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"cpus": cpus})
}

// handleFreeze forwards to FluxVM's own POST /v1/vms/{id}/freeze.
func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	if err := s.Flux.Freeze(r.Context(), r.PathValue("runtimeID")); err != nil {
		http.Error(w, fmt.Sprintf("freeze: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleThaw forwards to FluxVM's own POST /v1/vms/{id}/thaw.
func (s *Server) handleThaw(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	if err := s.Flux.Thaw(r.Context(), r.PathValue("runtimeID")); err != nil {
		http.Error(w, fmt.Sprintf("thaw: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleFrozen forwards to FluxVM's own GET /v1/vms/{id}/frozen.
func (s *Server) handleFrozen(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	frozen, err := s.Flux.IsFrozen(r.Context(), r.PathValue("runtimeID"))
	if err != nil {
		http.Error(w, fmt.Sprintf("frozen: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"frozen": frozen})
}
