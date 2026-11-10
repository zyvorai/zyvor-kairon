// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// requireCatalogAdmin runs the checks every mutating catalog route shares:
// admin-only (registering/removing/exporting an arbitrary node-local
// catalog entry is at least as sensitive as building a sandbox template,
// which FluxVM's own build_template already requires admin for), and the
// deployment must have the console relay configured. Returns the node's
// InternalIP on success, having already written a response on failure.
func (s *Server) requireCatalogAdmin(w http.ResponseWriter, r *http.Request, nodeName string) (nodeAddr string, ok bool) {
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi catalog denied: not an admin account", "username", username, "node", nodeName)
		}
		writeError(w, http.StatusForbidden, "modifying the image catalog requires an admin account")
		return "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the image catalog API is not enabled on this deployment")
		return "", false
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return "", false
	}
	return nodeAddr, true
}

// catalogTimeout is generous relative to execRelayClientTimeout (5min15s):
// AddCatalogEntry downloads and verifies a real image on the node,
// genuinely slower than any other synchronous relay call in this file.
const catalogTimeout = 15*time.Minute + 15*time.Second

// handleListCatalog lists a node's image catalog: kairon-ui -> kairon-node
// (internal/consoleproxy) -> FluxVM's own GET /v1/images/catalog. Any
// authenticated operator -- read-only visibility, the same posture
// handleListNodeSandboxes/handleListTemplates already have.
func (s *Server) handleListCatalog(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the image catalog API is not enabled on this deployment")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "catalog", &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type addCatalogEntryRequest struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Format string `json:"format,omitempty"`
}

// handleAddCatalogEntry registers a new catalog entry: kairon-ui ->
// kairon-node -> FluxVM's own POST /v1/images/catalog.
func (s *Server) handleAddCatalogEntry(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req addCatalogEntryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), catalogTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi catalog add requested", "username", usernameFromContext(r.Context()), "node", nodeName, "name", req.Name)
	}
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRemoveCatalogEntry removes a catalog entry: kairon-ui ->
// kairon-node -> FluxVM's own DELETE /v1/images/catalog/{name}.
func (s *Server) handleRemoveCatalogEntry(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi catalog remove requested", "username", usernameFromContext(r.Context()), "node", nodeName, "name", name)
	}
	if err := s.relayToNodeDelete(ctx, nodeAddr, "catalog/"+name); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// relayToNodeDelete is relayToNode's DELETE counterpart -- for a node
// relay call with no request or response body, expecting FluxVM's own
// REST convention of 204 No Content on success (unlike relayToNode/
// relayToNodeGet's plain-200 JSON responses).
func (s *Server) relayToNodeDelete(ctx context.Context, nodeAddr, nodePath string) error {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = s.nodeTransport()
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, upstreamURL, nil)
	if err != nil {
		return err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("connect to node relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		var upstreamErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&upstreamErr)
		msg := upstreamErr.Error
		if msg == "" {
			msg = fmt.Sprintf("node relay returned HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

type renameCatalogEntryRequest struct {
	NewName string `json:"newName"`
}

// handleRenameCatalogEntry renames a catalog entry: kairon-ui ->
// kairon-node -> FluxVM's own POST /v1/images/catalog/{name}/rename.
func (s *Server) handleRenameCatalogEntry(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req renameCatalogEntryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog/"+name+"/rename", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type cloneCatalogEntryRequest struct {
	TargetName string `json:"targetName"`
}

// handleCloneCatalogEntry clones a catalog entry: kairon-ui -> kairon-node
// -> FluxVM's own POST /v1/images/catalog/{name}/clone.
func (s *Server) handleCloneCatalogEntry(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req cloneCatalogEntryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), catalogTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog/"+name+"/clone", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type exportCatalogEntryRequest struct {
	Path string `json:"path"`
}

// handleExportCatalogEntry exports a catalog entry's image to an explicit
// node-local path: kairon-ui -> kairon-node -> FluxVM's own
// POST /v1/images/catalog/{name}/export.
func (s *Server) handleExportCatalogEntry(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req exportCatalogEntryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), catalogTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog/"+name+"/export", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type setCatalogReadOnlyRequest struct {
	ReadOnly bool `json:"readOnly"`
}

// handleSetCatalogReadOnly toggles a catalog entry's read_only flag:
// kairon-ui -> kairon-node -> FluxVM's own
// POST /v1/images/catalog/{name}/read-only.
func (s *Server) handleSetCatalogReadOnly(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req setCatalogReadOnlyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog/"+name+"/read-only", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCleanCatalogDownloads removes orphaned download artifacts:
// kairon-ui -> kairon-node -> FluxVM's own
// POST /v1/images/catalog/clean.
func (s *Server) handleCleanCatalogDownloads(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "catalog/clean", struct{}{}, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
