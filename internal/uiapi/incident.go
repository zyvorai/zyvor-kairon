// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/fluxvm"
)

// handleStartDiagnostics starts a FluxVM incident bundle for a Machine:
// kairon-ui -> kairon-node -> FluxVM POST /v1/vms/{id}/diagnostics.
// Administrators only (see requireIncidentAdmin): a bundle can hold a
// memory dump and, with redact off, the guest's secrets.
func (s *Server) handleStartDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.requireIncidentAdmin(w, r) {
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	var req fluxvm.DiagnosticsRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid diagnostics request: "+err.Error())
		return
	}
	body, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Log != nil {
		s.Log.Info("incident bundle requested", "username", usernameFromContext(r.Context()),
			"namespace", namespace, "machine", name, "memoryDump", req.MemoryDump, "redact", req.Redact, "pcapSeconds", req.PcapSeconds)
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayDiagnostics(w, ctx, http.MethodPost, nodeAddr, "diagnostics/"+url.PathEscape(runtimeID), body)
}

// handleGetDiagnostics reports one bundle job.
func (s *Server) handleGetDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.requireIncidentAdmin(w, r) {
		return
	}
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, r.PathValue("namespace"), r.PathValue("name"))
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	s.relayDiagnostics(w, ctx, http.MethodGet, nodeAddr, "diagnostics/"+url.PathEscape(runtimeID)+"/"+url.PathEscape(r.PathValue("token")), nil)
}

// handleDiagnosticsFile streams a finished bundle (tar.gz) or its guest
// memory dump. No timeout beyond the client's: a dump is as large as the
// guest's memory.
func (s *Server) handleDiagnosticsFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireIncidentAdmin(w, r) {
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	file := r.PathValue("file")
	if file != "bundle" && file != "memory-dump" {
		writeError(w, http.StatusNotFound, "unknown diagnostics file")
		return
	}
	if file == "memory-dump" && s.Log != nil {
		s.Log.Info("memory dump downloaded", "username", usernameFromContext(r.Context()), "namespace", namespace, "machine", name)
	}
	s.relayDiagnostics(w, r.Context(), http.MethodGet, nodeAddr,
		"diagnostics/"+url.PathEscape(runtimeID)+"/"+url.PathEscape(r.PathValue("token"))+"/"+file, nil)
}

// requireIncidentAdmin allows administrator accounts and the shared UI
// token, which is root-equivalent on every route (see handleResetPassword).
func (s *Server) requireIncidentAdmin(w http.ResponseWriter, r *http.Request) bool {
	username := usernameFromContext(r.Context())
	if username == "" || s.isAdminIdentity(r.Context(), username) {
		return true
	}
	if s.Log != nil {
		s.Log.Warn("incident bundle denied: not an admin account", "username", username,
			"namespace", r.PathValue("namespace"), "name", r.PathValue("name"))
	}
	writeError(w, http.StatusForbidden, "incident bundles require an admin account")
	return false
}

// relayDiagnostics calls kairon-node's relay and passes status, headers and
// body through, so FluxVM's 404/409 and refusal messages reach the client.
func (s *Server) relayDiagnostics(w http.ResponseWriter, ctx context.Context, method, nodeAddr, nodePath string, body []byte) {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath), rd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		writeError(w, http.StatusBadGateway, "node relay rejected the console token")
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
