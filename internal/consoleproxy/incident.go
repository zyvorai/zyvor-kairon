// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxDiagnosticsRequest bounds a bundle request body.
const maxDiagnosticsRequest = 64 << 10

// handleStartDiagnostics starts a FluxVM incident bundle.
// POST /v1/vms/{id}/diagnostics.
func (s *Server) handleStartDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	s.relayDiagnostics(w, r, http.MethodPost, "", io.LimitReader(r.Body, maxDiagnosticsRequest))
}

// handleGetDiagnostics reports one bundle job.
// GET /v1/vms/{id}/diagnostics/{token}.
func (s *Server) handleGetDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	s.relayDiagnostics(w, r, http.MethodGet, "/"+url.PathEscape(r.PathValue("token")), nil)
}

// handleDiagnosticsFile streams a finished bundle or its memory dump.
// GET /v1/vms/{id}/diagnostics/{token}/{bundle|memory-dump}.
func (s *Server) handleDiagnosticsFile(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	file := r.PathValue("file")
	if file != "bundle" && file != "memory-dump" {
		http.Error(w, "unknown diagnostics file", http.StatusNotFound)
		return
	}
	s.relayDiagnostics(w, r, http.MethodGet, "/"+url.PathEscape(r.PathValue("token"))+"/"+file, nil)
}

// relayDiagnostics passes FluxVM's status, headers and body through
// unchanged, so 404/409 and FluxVM's refusal messages reach the caller.
func (s *Server) relayDiagnostics(w http.ResponseWriter, r *http.Request, method, suffix string, body io.Reader) {
	resp, err := s.Flux.Diagnostics(r.Context(), method, r.PathValue("runtimeID"), suffix, body)
	if err != nil {
		http.Error(w, fmt.Sprintf("diagnostics: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyDiagnosticsResponse(w, resp)
}

func copyDiagnosticsResponse(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
