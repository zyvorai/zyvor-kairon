// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"context"
	"net/http"
)

type egressCheckRequest struct {
	Host string `json:"host"`
}

// egressCheckResponse deliberately does NOT carry FluxVM's own
// inject_authorization field verbatim -- that field is a real credential
// vault secret (the literal Authorization header value FluxVM would
// inject for this host), not a boolean, and returning it to any caller
// who can guess or enumerate a configured host would defeat the point of
// having a vault at all. WouldInjectCredential reports only whether one
// exists, never what it is.
type egressCheckResponse struct {
	Allow                 bool   `json:"allow"`
	Reason                string `json:"reason"`
	WouldInjectCredential bool   `json:"wouldInjectCredential"`
}

// handleEgressCheck asks a node's FluxVM whether a sandbox's outbound
// request to a given host would be allowed: kairon-ui -> kairon-node ->
// FluxVM's own POST /v1/egress/check, a stateless diagnostic against that
// node's static [sandbox] egress config (no runtime-mutable allowlist
// exists to manage here -- this is read-only, by design). Admin-only:
// even with the credential redacted above, confirming which hosts are
// allowlisted (and which have a vault credential configured at all) is
// still real information about a node's sandbox egress policy an
// operator shouldn't need to probe from outside an admin role.
func (s *Server) handleEgressCheck(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi egress-check denied: not an admin account", "username", username, "node", nodeName)
		}
		writeError(w, http.StatusForbidden, "the egress check requires an admin account")
		return
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "the egress check is not enabled on this deployment")
		return
	}
	var req egressCheckRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var decision struct {
		Allow               bool   `json:"allow"`
		InjectAuthorization string `json:"inject_authorization"`
		Reason              string `json:"reason"`
	}
	if err := s.relayToNode(ctx, nodeAddr, "egress-check", req, &decision); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, egressCheckResponse{
		Allow:                 decision.Allow,
		Reason:                decision.Reason,
		WouldInjectCredential: decision.InjectAuthorization != "",
	})
}
