// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// handleListCatalog forwards to FluxVM's own GET /v1/images/catalog --
// node-scoped, like sandbox templates: an entry registered on one node is
// invisible from another.
func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	items, err := s.Flux.ListCatalog(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("list catalog: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

type addCatalogEntryRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Format string `json:"format,omitempty"`
}

// handleAddCatalogEntry forwards to FluxVM's own POST /v1/images/catalog.
func (s *Server) handleAddCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req addCatalogEntryRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	// Downloading and verifying a real image (AddCatalogEntry's own
	// server-side work) is genuinely slower than any other synchronous
	// relay call in this file except building a sandbox template --
	// reuses that same generous timeout.
	ctx, cancel := context.WithTimeout(r.Context(), buildTemplateTimeout)
	defer cancel()
	entry, err := s.Flux.AddCatalogEntry(ctx, req.Name, req.Source, req.Format)
	if err != nil {
		http.Error(w, fmt.Sprintf("add catalog entry: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(entry)
}

// handleRemoveCatalogEntry forwards to FluxVM's own
// DELETE /v1/images/catalog/{name}.
func (s *Server) handleRemoveCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	if err := s.Flux.RemoveCatalogEntry(r.Context(), r.PathValue("name")); err != nil {
		http.Error(w, fmt.Sprintf("remove catalog entry: %v", err), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type renameCatalogEntryRequest struct {
	NewName string `json:"newName"`
}

// handleRenameCatalogEntry forwards to FluxVM's own
// POST /v1/images/catalog/{name}/rename.
func (s *Server) handleRenameCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req renameCatalogEntryRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	entry, err := s.Flux.RenameCatalogEntry(r.Context(), r.PathValue("name"), req.NewName)
	if err != nil {
		http.Error(w, fmt.Sprintf("rename catalog entry: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(entry)
}

type cloneCatalogEntryRequest struct {
	TargetName string `json:"targetName"`
}

// handleCloneCatalogEntry forwards to FluxVM's own
// POST /v1/images/catalog/{name}/clone.
func (s *Server) handleCloneCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req cloneCatalogEntryRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	entry, err := s.Flux.CloneCatalogEntry(r.Context(), r.PathValue("name"), req.TargetName)
	if err != nil {
		http.Error(w, fmt.Sprintf("clone catalog entry: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(entry)
}

type exportCatalogEntryRequest struct {
	Path string `json:"path"`
}

// handleExportCatalogEntry forwards to FluxVM's own
// POST /v1/images/catalog/{name}/export.
func (s *Server) handleExportCatalogEntry(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req exportCatalogEntryRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	if err := s.Flux.ExportCatalogEntry(r.Context(), r.PathValue("name"), req.Path); err != nil {
		http.Error(w, fmt.Sprintf("export catalog entry: %v", err), http.StatusBadGateway)
		return
	}
	// A plain 200 with a small JSON body, not 204 -- internal/uiapi's
	// relayToNode (shared with every other POST relay in this project)
	// treats any non-200 response as an error.
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

type setCatalogReadOnlyRequest struct {
	ReadOnly bool `json:"readOnly"`
}

// handleSetCatalogReadOnly forwards to FluxVM's own
// POST /v1/images/catalog/{name}/read-only.
func (s *Server) handleSetCatalogReadOnly(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req setCatalogReadOnlyRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	entry, err := s.Flux.SetCatalogReadOnly(r.Context(), r.PathValue("name"), req.ReadOnly)
	if err != nil {
		http.Error(w, fmt.Sprintf("set catalog read-only: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(entry)
}

// handleCleanCatalogDownloads forwards to FluxVM's own
// POST /v1/images/catalog/clean.
func (s *Server) handleCleanCatalogDownloads(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	removed, err := s.Flux.CleanCatalogDownloads(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("clean catalog: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"removed": removed})
}
