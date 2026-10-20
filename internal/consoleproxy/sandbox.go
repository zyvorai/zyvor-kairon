// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package consoleproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// handleListSandboxes forwards to FluxVM's own GET /v1/sandboxes -- a
// node-scoped listing (sandboxes, like everything else in this file, are
// per-node state, never replicated across nodes), unlike every other
// route in this package which addresses one specific VM by runtimeID.
func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	items, err := s.Flux.ListSandboxes(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("list sandboxes: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// handleListTemplates forwards to FluxVM's own GET /v1/templates.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	items, err := s.Flux.ListTemplates(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("list templates: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// buildTemplateTimeout is generous relative to execRelayTimeout (5min):
// building a template pulls a real OCI image (skopeo) and exports its
// rootfs (umoci) -- genuinely slower than any other synchronous relay
// call in this package.
const buildTemplateTimeout = 15 * time.Minute

type buildTemplateRequest struct {
	Name     string `json:"name"`
	ImageRef string `json:"imageRef"`
}

// handleBuildTemplate forwards to FluxVM's own POST /v1/templates, which
// exports an OCI image's rootfs (via skopeo+umoci) into a fast-boot
// sandbox template -- can take a real amount of time (an image pull plus
// an rootfs export), bounded by execRelayTimeout the same as every other
// synchronous relay call in this package.
func (s *Server) handleBuildTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	var req buildTemplateRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), buildTemplateTimeout)
	defer cancel()
	info, err := s.Flux.BuildTemplate(ctx, req.Name, req.ImageRef)
	if err != nil {
		http.Error(w, fmt.Sprintf("build template: %v", err), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(info)
}

// handleSandboxHTTPProxy relays an arbitrary HTTP request straight
// through to a sandbox's own guest HTTP server -- kairon-ui ->
// kairon-node -> FluxVM's own GET/POST/... /v1/sandboxes/{id}/http/{port}/{path}
// (any HTTP method, matching FluxVM's own `any(sandbox_http_proxy)`
// route), which itself dials the sandbox's guest_ip directly. Unlike
// every other route in this package, method/headers/body/status all pass
// through mostly unmodified -- this is a generic reverse proxy, not a
// typed JSON request/response. The shared bearer token used for this
// hop (checkToken) is deliberately never forwarded to the guest; FluxVM's
// own token (if configured) replaces it instead, the same substitution
// every other relay call in this package already does via s.Flux itself.
func (s *Server) handleSandboxHTTPProxy(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(w, r) {
		return
	}
	runtimeID := r.PathValue("runtimeID")
	port := r.PathValue("port")
	rest := r.PathValue("rest")
	upstreamURL := s.Flux.BaseURL + "/v1/sandboxes/" + url.PathEscape(runtimeID) + "/http/" + url.PathEscape(port) + "/" + rest
	if q := r.URL.RawQuery; q != "" {
		upstreamURL += "?" + q
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, vv := range r.Header {
		if k == "Authorization" {
			continue
		}
		for _, v := range vv {
			upstreamReq.Header.Add(k, v)
		}
	}
	if s.Flux.Token != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+s.Flux.Token)
	}
	resp, err := s.Flux.HTTP.Do(upstreamReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial fluxvm sandbox proxy: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
