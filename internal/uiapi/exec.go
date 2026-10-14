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

	"github.com/zyvorai/kairon/internal/model"
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
	m, username, nodeAddr, ok := s.execGate(w, r)
	if !ok {
		return
	}
	var req execRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if s.Log != nil {
		// Deliberately not logging req.Path/Args/Powershell -- a command's
		// own arguments can carry secrets (e.g. a token passed on the
		// command line), and this audit line's job is accountability (who
		// ran something, on what, when), not a transcript of what was run.
		s.Log.Info("uiapi exec requested", "username", username, "namespace", m.Namespace(), "name", m.Metadata.Name)
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	out, err := s.relayExec(ctx, nodeAddr, m.Status.RuntimeID, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSoftReboot asks the guest OS to reboot itself through the guest
// agent (systemd/sysvinit, else PowerShell for Windows), like virtctl
// soft-reboot. Same gates as handleExec.
func (s *Server) handleSoftReboot(w http.ResponseWriter, r *http.Request) {
	m, username, nodeAddr, ok := s.execGate(w, r)
	if !ok {
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi soft reboot requested", "username", username, "namespace", m.Namespace(), "name", m.Metadata.Name)
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	t := uint64(15)
	out, err := s.relayExec(ctx, nodeAddr, m.Status.RuntimeID, execRequest{Path: "/bin/sh", Args: []string{"-c", "systemctl reboot || reboot || shutdown -r now"}, TimeoutSeconds: &t})
	if err != nil || out.ExitCode != 0 {
		out, err = s.relayExec(ctx, nodeAddr, m.Status.RuntimeID, execRequest{Powershell: "Restart-Computer -Force", TimeoutSeconds: &t})
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if out.ExitCode != 0 {
		writeError(w, http.StatusBadGateway, "guest refused to reboot: "+strings.TrimSpace(out.Stderr+" "+out.Stdout))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// execGate applies the guest exec gates: admin account, console allowlist,
// relay enabled, a Running Machine with a runtime and a guest agent. It
// writes the error response itself when ok is false.
func (s *Server) execGate(w http.ResponseWriter, r *http.Request) (m model.Machine, username, nodeAddr string, ok bool) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return m, "", "", false
	}
	username = usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi exec denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "guest exec requires an admin account")
		return m, "", "", false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi exec denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to exec into this machine")
		return m, "", "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "exec is not enabled on this deployment")
		return m, "", "", false
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
		return m, "", "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return m, "", "", false
	}
	if !m.Spec.GuestAgent.Enabled {
		writeError(w, http.StatusBadRequest, "spec.guestAgent.enabled is required for guest exec")
		return m, "", "", false
	}
	nodeAddr, err = s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return m, "", "", false
	}
	return m, username, nodeAddr, true
}

// relayExec runs req through kairon-node's exec relay.
func (s *Server) relayExec(ctx context.Context, nodeAddr, runtimeID string, req execRequest) (execResponse, error) {
	var out execResponse
	scheme := "http"
	transport := http.DefaultTransport
	if s.ConsoleTLS != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: s.ConsoleTLS}
	}
	upstreamURL := fmt.Sprintf("%s://%s:%s/exec/%s", scheme, nodeAddr, s.ConsolePort, runtimeID)
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+s.ConsoleToken)
	upstreamReq.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(upstreamReq)
	if err != nil {
		return out, fmt.Errorf("connect to node exec relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("%s", relayErrorMessage(resp, "node exec relay"))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode node exec relay response: %w", err)
	}
	return out, nil
}

// handleRestartMachine asks kairon-node to power-cycle the Machine (see
// model.AnnotationRestartRequested).
func (s *Server) handleRestartMachine(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if m.DesiredPowerState() != "Running" {
		writeError(w, http.StatusConflict, "restart needs spec.powerState Running; start the machine instead")
		return
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		model.AnnotationRestartRequested: time.Now().UTC().Format(time.RFC3339Nano),
	}}}
	if err := s.Kube.PatchMachine(r.Context(), namespace, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	if s.Log != nil {
		s.Log.Info("uiapi restart requested", "username", usernameFromContext(r.Context()), "namespace", namespace, "name", name)
	}
	w.WriteHeader(http.StatusNoContent)
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
