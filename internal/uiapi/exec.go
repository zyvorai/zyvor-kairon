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
	"strings"
	"time"
)

// execRequest/execResponse mirror internal/consoleproxy's own wire shape
// (which in turn mirrors internal/fluxvm.QGAExecRequest/Result) -- kept as
// separate types rather than shared ones so kairon-ui's public HTTP
// contract with the browser doesn't change just because an internal
// package's struct layout does.
type execRequest struct {
	Path           string   `json:"path,omitempty"`
	Args           []string `json:"args,omitempty"`
	Powershell     string   `json:"powershell,omitempty"`
	TimeoutSeconds *uint64  `json:"timeoutSeconds,omitempty"`
}

type execResponse struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// execRelayClientTimeout bounds kairon-ui's own wait on kairon-node's
// relay -- comfortably above internal/consoleproxy's own execRelayTimeout
// (5 minutes), so a legitimate slow command has room to actually finish
// there before this hop gives up first.
const execRelayClientTimeout = 5*time.Minute + 15*time.Second

// handleExec relays a guest-exec request to the target Machine's node:
// kairon-ui -> kairon-node (internal/consoleproxy) -> FluxVM's real
// qemu-guest-agent guest-exec. Unlike the VNC console (any authenticated
// operator by default, unless the annotation allowlist says otherwise),
// this requires an admin account outright -- running arbitrary code inside
// a guest is a meaningfully bigger capability than viewing its screen, and
// this project has no finer-grained permission for it yet (a real,
// documented limit, not an oversight -- see SECURITY.md). An OIDC-only
// identity with no local admin record is denied for the same reason
// RBACConsoleCheck already documents for OIDC-console access: there is no
// group-to-admin claim mapping in this project yet.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi exec denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "guest exec requires an admin account")
		return
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi exec denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to exec into this machine")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "exec is not enabled on this deployment")
		return
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
		return
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return
	}
	if !m.Spec.GuestAgent.Enabled {
		writeError(w, http.StatusBadRequest, "spec.guestAgent.enabled is required for guest exec")
		return
	}
	var req execRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/exec/%s", scheme, nodeAddr, s.ConsolePort, m.Status.RuntimeID)
	body, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	upstreamReq.Header.Set("Content-Type", "application/json")

	if s.Log != nil {
		// Deliberately not logging req.Path/Args/Powershell -- a command's
		// own arguments can carry secrets (e.g. a token passed on the
		// command line), and this audit line's job is accountability (who
		// ran something, on what, when), not a transcript of what was run.
		s.Log.Info("uiapi exec requested", "username", username, "namespace", namespace, "name", name)
	}
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "connect to node exec relay: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, relayErrorMessage(resp, "node exec relay"))
		return
	}
	var out execResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeError(w, http.StatusBadGateway, "decode node exec relay response: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// relayErrorMessage is the reason a node relay gave for a non-200: its JSON
// "error", else its plain-text body (kairon-node's relay answers with
// http.Error), else just the status.
func relayErrorMessage(resp *http.Response, relay string) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var upstreamErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &upstreamErr) == nil && upstreamErr.Error != "" {
		return upstreamErr.Error
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		return text
	}
	return fmt.Sprintf("%s returned HTTP %d", relay, resp.StatusCode)
}
