// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"net/http"

	"github.com/zyvorai/kairon/internal/model"
)

// fsfreezeStatusResponse/firewallOpenRequest/firewallCloseRequest/
// firewallResponse mirror internal/consoleproxy's own wire shape, the
// same reason execRequest/execResponse do (internal/uiapi/exec.go).
type fsfreezeStatusResponse struct {
	Status string `json:"status"`
}

type firewallOpenRequest struct {
	Name           string  `json:"name"`
	Port           uint16  `json:"port"`
	Protocol       string  `json:"protocol,omitempty"`
	TimeoutSeconds *uint64 `json:"timeoutSeconds,omitempty"`
}

type firewallCloseRequest struct {
	Name           string  `json:"name"`
	TimeoutSeconds *uint64 `json:"timeoutSeconds,omitempty"`
}

type firewallResponse struct {
	ExitCode int64  `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// requireQGAAccess runs the same checks handleExec already does
// (internal/uiapi/exec.go): admin-only, consoleAuthorized, the console
// relay configured, the Machine Running with spec.guestAgent.enabled --
// fsfreeze-status and the firewall toggle both ride the same
// qemu-guest-agent channel guest-exec does, so they get the same gate.
func (s *Server) requireQGAAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (m model.Machine, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return m, false
	}
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi qga denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "this requires an admin account")
		return m, false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi qga denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to access this machine")
		return m, false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "this is not enabled on this deployment")
		return m, false
	}
	if m.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machine is not Running")
		return m, false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return m, false
	}
	if !m.Spec.GuestAgent.Enabled {
		writeError(w, http.StatusBadRequest, "spec.guestAgent.enabled is required")
		return m, false
	}
	return m, true
}

// handleQGAFsfreezeStatus is a read-only diagnostic: what does
// qemu-guest-agent itself currently report for this Machine's guest
// filesystem freeze state -- kairon-ui -> kairon-node
// (internal/consoleproxy) -> FluxVM's GET /v1/vms/{id}/qga/fsfreeze-status.
func (s *Server) handleQGAFsfreezeStatus(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireQGAAccess(w, r, namespace, name)
	if !ok {
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out fsfreezeStatusResponse
	if err := s.relayToNodeGet(ctx, nodeAddr, "qga-fsfreeze-status/"+m.Status.RuntimeID, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleQGAFirewallOpen/handleQGAFirewallClose toggle a named firewall
// rule inside the guest via qemu-guest-agent -- same relay shape as
// handleExec/handleAgentPutFile, just a different upstream path. Both
// share handleQGAFirewallToggle below; the only real difference between
// them is the request body's shape and the "open"/"close" relay path
// segment.
func (s *Server) handleQGAFirewallOpen(w http.ResponseWriter, r *http.Request) {
	handleQGAFirewallToggle[firewallOpenRequest](s, w, r, "open")
}

func (s *Server) handleQGAFirewallClose(w http.ResponseWriter, r *http.Request) {
	handleQGAFirewallToggle[firewallCloseRequest](s, w, r, "close")
}

// handleQGAFirewallToggle implements handleQGAFirewallOpen/Close: Req is
// the request body's concrete type (firewallOpenRequest/
// firewallCloseRequest) and action is the "open"/"close" relay path
// segment.
func handleQGAFirewallToggle[Req any](s *Server, w http.ResponseWriter, r *http.Request, action string) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, ok := s.requireQGAAccess(w, r, namespace, name)
	if !ok {
		return
	}
	var req Req
	if err := decodeJSON(w, r, &req); err != nil {
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
	var out firewallResponse
	if err := s.relayToNode(ctx, nodeAddr, "qga-firewall/"+action+"/"+m.Status.RuntimeID, req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
