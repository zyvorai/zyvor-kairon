// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// handleListNodeSandboxes lists every FluxVM sandbox on one node: kairon-ui
// -> kairon-node (internal/consoleproxy) -> FluxVM's own GET
// /v1/sandboxes. Node-scoped, not Machine-scoped -- a sandbox created
// directly against FluxVM (outside a Kairon Machine entirely) still shows
// up here, the same "this node's real state, not just what Kairon
// created" posture GET /api/v1/machines doesn't have to worry about since
// every Machine already has a Kubernetes object backing it. Any
// authenticated operator, same posture as viewing the machines list --
// this is read-only visibility, not an action.
func (s *Server) handleListNodeSandboxes(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "sandboxes are not enabled on this deployment")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.relayToNodeGet(ctx, nodeAddr, "sandboxes", &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleListTemplates lists every sandbox template built on one node --
// FluxVM's own GET /v1/templates. Same any-operator visibility posture as
// handleListNodeSandboxes.
func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "sandboxes are not enabled on this deployment")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := s.relayToNodeGet(ctx, nodeAddr, "templates", &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// buildTemplateTimeout is generous: building a template pulls a real OCI
// image and exports its rootfs on the node -- genuinely slower than any
// other relay call in this package.
const buildTemplateTimeout = 15*time.Minute + 15*time.Second

type buildTemplateRequest struct {
	Name     string `json:"name"`
	ImageRef string `json:"imageRef"`
}

// handleBuildTemplate builds a new sandbox template from an OCI image
// reference: kairon-ui -> kairon-node -> FluxVM's own POST /v1/templates.
// Admin-only -- FluxVM's own build_template handler requires an admin
// role itself, and pulling/exporting an arbitrary OCI image is a real
// resource cost an operator shouldn't be able to trigger freely.
func (s *Server) handleBuildTemplate(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi build-template denied: not an admin account", "username", username, "node", nodeName)
		}
		writeError(w, http.StatusForbidden, "building a sandbox template requires an admin account")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "sandboxes are not enabled on this deployment")
		return
	}
	var req buildTemplateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), buildTemplateTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi build-template requested", "username", username, "node", nodeName, "name", req.Name, "imageRef", req.ImageRef)
	}
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "templates", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// requireSandboxAccess runs the checks handleSandboxHTTPProxy needs
// before touching FluxVM at all: admin-only (this proxies arbitrary
// HTTP requests -- method, path, body -- straight into whatever the
// guest's own web server does with them, a materially different and
// broader capability than viewing a display or running one command), the
// target Machine must actually be a sandbox (spec.sandbox set) and
// Running. Deliberately does not gate on spec.guestAgent.console --
// unlike guest file access/agent-exec, this never touches the vsock
// agent at all; it dials the guest's own network IP directly, the same
// way FluxVM's own sandbox_http_proxy handler does.
func (s *Server) requireSandboxAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (nodeAddr, runtimeID, username string, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return "", "", "", false
	}
	username = usernameFromContext(r.Context())
	if !s.guestExecAllowed(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi sandbox-http denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "the sandbox HTTP proxy requires an admin account")
		return "", "", "", false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi sandbox-http denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to reach this machine's sandbox HTTP proxy")
		return "", "", "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the sandbox HTTP proxy is not enabled on this deployment")
		return "", "", "", false
	}
	if m.Spec.Sandbox == nil {
		writeError(w, http.StatusBadRequest, "spec.sandbox is required for the sandbox HTTP proxy")
		return "", "", "", false
	}
	if m, _, err = s.ensureAwake(r.Context(), m); err != nil {
		writeWakeError(w, err)
		return "", "", "", false
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
		return "", "", "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return "", "", "", false
	}
	nodeAddr, err = s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return "", "", "", false
	}
	return nodeAddr, m.Status.RuntimeID, username, true
}

// handleSandboxHTTPProxy relays an arbitrary HTTP request through to a
// sandbox Machine's own guest HTTP server: browser/API client ->
// kairon-ui -> kairon-node (internal/consoleproxy) -> FluxVM's own
// GET/POST/... /v1/sandboxes/{id}/http/{port}/{path}. Method, headers
// (except this hop's own Authorization), body, and the guest's response
// status/headers/body all pass through -- see
// internal/consoleproxy.handleSandboxHTTPProxy's own doc comment for why
// this is a raw relay rather than a typed JSON call like every other
// route in this file.
func (s *Server) handleSandboxHTTPProxy(w http.ResponseWriter, r *http.Request) {
	namespace, name, port, rest := r.PathValue("namespace"), r.PathValue("name"), r.PathValue("port"), r.PathValue("rest")
	nodeAddr, runtimeID, username, ok := s.requireSandboxAccess(w, r, namespace, name)
	if !ok {
		return
	}
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/sandbox-proxy/%s/%s/%s", scheme, nodeAddr, s.ConsolePort, runtimeID, port, rest)
	if q := r.URL.RawQuery; q != "" {
		upstreamURL += "?" + q
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	upstreamReq, err := http.NewRequestWithContext(ctx, r.Method, upstreamURL, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for k, vv := range r.Header {
		for _, v := range vv {
			upstreamReq.Header.Add(k, v)
		}
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	if s.Log != nil {
		s.Log.Info("uiapi sandbox-http requested", "username", username, "namespace", namespace, "name", name, "port", port)
	}
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node sandbox proxy: "+err.Error())
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
