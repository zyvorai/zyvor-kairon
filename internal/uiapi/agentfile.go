// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zyvorai/kairon/internal/model"
)

// maxAgentFileBodyBytes overrides decodeJSON's own defaultMaxRequestBodyBytes
// for handleAgentPutFile -- closing the one half of the gap
// machine-guest-agent-files.md's "Real limits" section used to document
// honestly: a read was always capped (~3MB of real file content,
// indirectly, via internal/fluxvm.Client's own 4MiB HTTP response limit),
// but "Writes have no such cap enforced on Kairon's side" -- before
// decodeJSON gained any body-size limit at all, a write's ContentBase64
// could be arbitrarily large. Sized to comfortably fit that same ~3MB
// real-content ceiling once base64-encoded (~4/3 expansion) plus a
// generous JSON envelope allowance, matching the read side's own limit
// rather than picking an unrelated number.
const maxAgentFileBodyBytes = 6 << 20 // 6MiB

// agentPutFileRequest/agentGetFileRequest/agentFileResponse mirror
// internal/consoleproxy's own wire shape for the same reason
// execRequest/execResponse do (internal/uiapi/exec.go's own doc comment).
type agentPutFileRequest struct {
	Path          string  `json:"path"`
	ContentBase64 string  `json:"contentBase64"`
	Mode          *uint32 `json:"mode,omitempty"`
}

type agentGetFileRequest struct {
	Path string `json:"path"`
}

type agentFileResponse struct {
	ContentBase64 string `json:"contentBase64,omitempty"`
	Mode          uint32 `json:"mode,omitempty"`
}

// requireGuestFileAccess runs the checks handleAgentPutFile/
// handleAgentGetFile share before touching FluxVM at all: same admin-only
// gate as handleExec (arbitrary guest file read/write is at least as
// sensitive as arbitrary guest command execution), but gated on
// spec.guestAgent.console rather than spec.guestAgent.enabled -- this
// rides FluxVM's own bespoke vsock guest agent (the same one the
// interactive text console uses), not qemu-guest-agent. Returns the
// Machine and the caller's username on success, having already written a
// response on failure.
func (s *Server) requireGuestFileAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (m model.Machine, username string, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return m, "", false
	}
	username = usernameFromContext(r.Context())
	if !s.guestExecAllowed(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi agent-file denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "guest file access requires an admin account")
		return m, "", false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi agent-file denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to access this machine's guest files")
		return m, "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "guest file access is not enabled on this deployment")
		return m, "", false
	}
	m, woke, err := s.ensureAwake(r.Context(), m)
	if err != nil {
		writeWakeError(w, err)
		return m, "", false
	}
	if woke {
		if nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName); err == nil {
			s.waitForGuestAgent(r.Context(), nodeAddr, m)
		}
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
		return m, "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return m, "", false
	}
	if !m.Spec.GuestAgent.Console {
		writeError(w, http.StatusBadRequest, "spec.guestAgent.console is required for guest file access")
		return m, "", false
	}
	return m, username, true
}

// relayToNode POSTs body as JSON to kairon-node's own relay at nodePath
// (e.g. "agent-file/put/<runtimeID>") and decodes a JSON response into
// out. Shares handleExec's own scheme/transport/error-shape conventions
// (internal/uiapi/exec.go), duplicated rather than factored out -- the two
// handlers already accepted this same duplication between handleConsole
// and handleExec.
func (s *Server) relayToNode(ctx context.Context, nodeAddr, nodePath string, body, out any) error {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath)
	reqBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	upstreamReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("connect to node relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", relayErrorMessage(resp, "node relay"))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode node relay response: %w", err)
	}
	return nil
}

// relayToNodeGet is relayToNode's GET counterpart -- for a read-only node
// relay call that has no request body (e.g. handleQGAFsfreezeStatus).
func (s *Server) relayToNodeGet(ctx context.Context, nodeAddr, nodePath string, out any) error {
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/%s", scheme, nodeAddr, s.ConsolePort, nodePath)
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if err != nil {
		return err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		return fmt.Errorf("connect to node relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", relayErrorMessage(resp, "node relay"))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode node relay response: %w", err)
	}
	return nil
}

// handleAgentPutFile writes a file into the guest: kairon-ui -> kairon-node
// (internal/consoleproxy) -> FluxVM's own bespoke vsock guest agent
// (POST /v1/vms/{id}/agent/put-file). See requireGuestFileAccess's own doc
// comment for the authorization model.
func (s *Server) handleAgentPutFile(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, username, ok := s.requireGuestFileAccess(w, r, namespace, name)
	if !ok {
		return
	}
	var req agentPutFileRequest
	if err := decodeJSONWithLimit(w, r, &req, maxAgentFileBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	if s.Log != nil {
		// Deliberately not logging req.Path -- a file's own path can be
		// sensitive on its own (e.g. naming a credential file), matching
		// handleExec's own reasoning for not logging command arguments.
		s.Log.Info("uiapi agent put-file requested", "username", username, "namespace", namespace, "name", name)
	}
	nodePath := "agent-file/put/" + m.Status.RuntimeID
	if err := s.relayToNode(ctx, nodeAddr, nodePath, req, &agentFileResponse{}); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAgentGetFile reads a file from the guest -- handleAgentPutFile's
// read counterpart.
func (s *Server) handleAgentGetFile(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, username, ok := s.requireGuestFileAccess(w, r, namespace, name)
	if !ok {
		return
	}
	var req agentGetFileRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	var out agentFileResponse
	relayGuestAgentRequest(s, w, r, m, req, &out, "agent-file/get/"+m.Status.RuntimeID,
		"uiapi agent get-file requested", username, namespace, name)
}
