// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"net/http"
)

// handleRuntimeCapabilities returns a node's real, current FluxVM
// capability manifest: kairon-ui -> kairon-node -> FluxVM's own
// GET /v1/runtime/capabilities. Any authenticated operator -- static,
// non-sensitive config, the same posture handleListNodeSandboxes/
// handleListTemplates/handleListCatalog already have for node visibility.
func (s *Server) handleRuntimeCapabilities(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "runtime capabilities are not enabled on this deployment")
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
	if err := s.relayToNodeGet(ctx, nodeAddr, "capabilities", &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// requireDiagnosticsAccess runs the checks handlePressure/handleCPUSet/
// handleFrozen share -- consoleAuthorized (any authenticated operator by
// default, the same read-only visibility posture handleLogs already
// has), a runtime must exist. Returns the target Machine's node address
// and runtime ID on success, having already written a response on
// failure.
func (s *Server) requireDiagnosticsAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (nodeAddr, runtimeID string, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return "", "", false
	}
	username := usernameFromContext(r.Context())
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi diagnostics denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to view this machine's diagnostics")
		return "", "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "diagnostics are not enabled on this deployment")
		return "", "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return "", "", false
	}
	nodeAddr, err = s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return "", "", false
	}
	return nodeAddr, m.Status.RuntimeID, true
}

// handlePressure returns a Machine's real, cgroup-derived PSI pressure
// stats: kairon-ui -> kairon-node -> FluxVM's own
// GET /v1/vms/{id}/pressure.
func (s *Server) handlePressure(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "pressure/"+runtimeID, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCPUSet returns the real, effective host CPU numbers a Machine's
// cgroup is currently allowed to run on: kairon-ui -> kairon-node ->
// FluxVM's own GET /v1/vms/{id}/cpuset.
func (s *Server) handleCPUSet(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "cpuset/"+runtimeID, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFrozen reports whether a Machine's cgroup is currently frozen:
// kairon-ui -> kairon-node -> FluxVM's own GET /v1/vms/{id}/frozen.
func (s *Server) handleFrozen(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireDiagnosticsAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "frozen/"+runtimeID, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// requireFreezeAccess is handleFreeze/handleThaw's own gate -- admin-only,
// unlike the read-only diagnostics above: cgroup-freezing a Machine's
// entire VMM process (not just its guest CPUs, the way spec.powerState:
// Paused does) is a materially more disruptive operation, the same
// posture guest exec/VM-state snapshot-restore already have for
// comparably impactful actions.
func (s *Server) requireFreezeAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (nodeAddr, runtimeID string, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return "", "", false
	}
	username := usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi freeze denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "freezing/thawing a machine requires an admin account")
		return "", "", false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi freeze denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to freeze/thaw this machine")
		return "", "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "freeze/thaw is not enabled on this deployment")
		return "", "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return "", "", false
	}
	nodeAddr, err = s.nodeInternalIP(r.Context(), m.Status.NodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return "", "", false
	}
	return nodeAddr, m.Status.RuntimeID, true
}

// handleFreeze halts a Machine's entire cgroup at the kernel scheduler
// level: kairon-ui -> kairon-node -> FluxVM's own
// POST /v1/vms/{id}/freeze. See internal/fluxvm.Client.Freeze's own doc
// comment for why this is genuinely different from spec.powerState:
// Paused.
func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireFreezeAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi freeze requested", "username", usernameFromContext(r.Context()), "namespace", namespace, "name", name)
	}
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "freeze/"+runtimeID, struct{}{}, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleThaw reverses handleFreeze: kairon-ui -> kairon-node -> FluxVM's
// own POST /v1/vms/{id}/thaw.
func (s *Server) handleThaw(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	nodeAddr, runtimeID, ok := s.requireFreezeAccess(w, r, namespace, name)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi thaw requested", "username", usernameFromContext(r.Context()), "namespace", namespace, "name", name)
	}
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "thaw/"+runtimeID, struct{}{}, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
