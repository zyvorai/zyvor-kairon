// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"net/http"

	"github.com/zyvorai/kairon/internal/model"
)

// vmSnapshotRequest/vmSnapshotResponse mirror internal/consoleproxy's own
// wire shape for the same reason execRequest/execResponse
// (internal/uiapi/exec.go) do. Deliberately named distinctly from
// internal/uiapi/snapshots.go's createSnapshotRequest/MachineSnapshot: that
// one drives CSI's disk-content-only VolumeSnapshot machinery
// (internal/csinode); this one is a full hypervisor-level VM-state
// checkpoint via FluxVM's own POST /v1/vms/{id}/snapshot -- RAM, CPU, and
// device state, unrelated to disk content.
type vmSnapshotRequest struct {
	Tag string `json:"tag"`
}

type vmSnapshotResponse struct {
	Status string `json:"status"`
}

// requireVMSnapshotAccess runs the checks handleVMSnapshot/
// handleVMRestoreSnapshot share: admin-only, same posture as guest exec and
// guest file access (internal/uiapi/exec.go, internal/uiapi/agentfile.go) --
// restoring a snapshot stops and restarts the VM in place, at least as
// disruptive as either of those. Unlike guest file access, this never
// touches a guest agent at all (QEMU/Cloud Hypervisor level, backend-
// agnostic against FluxVM itself), so there's no spec.guestAgent gate here.
func (s *Server) requireVMSnapshotAccess(w http.ResponseWriter, r *http.Request, namespace, name string) (m model.Machine, username string, ok bool) {
	m, err := s.Kube.GetMachine(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return m, "", false
	}
	username = usernameFromContext(r.Context())
	if !s.isAdminIdentity(r.Context(), username) {
		if s.Log != nil {
			s.Log.Warn("uiapi vm-snapshot denied: not an admin account", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "VM-state snapshot/restore requires an admin account")
		return m, "", false
	}
	if !s.consoleAuthorized(r.Context(), m, username) {
		if s.Log != nil {
			s.Log.Warn("uiapi vm-snapshot denied", "username", username, "namespace", namespace, "name", name)
		}
		writeError(w, http.StatusForbidden, "not authorized to snapshot/restore this machine")
		return m, "", false
	}
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "VM-state snapshot/restore is not enabled on this deployment")
		return m, "", false
	}
	if m.Status.RuntimeID == "" || m.Status.NodeName == "" {
		writeError(w, http.StatusConflict, "machine has no runtime yet")
		return m, "", false
	}
	return m, username, true
}

// handleVMSnapshot saves an in-place VM-state checkpoint: kairon-ui ->
// kairon-node (internal/consoleproxy) -> FluxVM's own
// POST /v1/vms/{id}/snapshot. Requires the VM to already be Running or
// Paused (FluxVM's own error surfaces unmodified otherwise, e.g. for a
// Firecracker-backed Machine, which doesn't support this at all); the VM
// keeps running/stays paused throughout.
func (s *Server) handleVMSnapshot(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, username, ok := s.requireVMSnapshotAccess(w, r, namespace, name)
	if !ok {
		return
	}
	if m.Status.Phase != "Running" && m.Status.Phase != "Paused" {
		writeError(w, http.StatusConflict, "machine must be Running or Paused to snapshot")
		return
	}
	var req vmSnapshotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Tag == "" {
		writeError(w, http.StatusBadRequest, "tag is required")
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
		s.Log.Info("uiapi vm snapshot requested", "username", username, "namespace", namespace, "name", name, "tag", req.Tag)
	}
	var out vmSnapshotResponse
	nodePath := "vm-snapshot/" + m.Status.RuntimeID
	if err := s.relayToNode(ctx, nodeAddr, nodePath, req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVMRestoreSnapshot restores a VM to exactly the state a prior
// handleVMSnapshot call captured: kairon-ui -> kairon-node
// (internal/consoleproxy) -> fluxvm.Client.RestoreSnapshot, which itself
// orchestrates FluxVM's own stop -> start-from-snapshot sequence (see
// internal/fluxvm/hibernate.go's own doc comments for why start-from-
// snapshot alone isn't enough -- it silently no-ops on an already-Running
// VM). The VM is always stopped and restarted, whatever its state going in.
func (s *Server) handleVMRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	m, username, ok := s.requireVMSnapshotAccess(w, r, namespace, name)
	if !ok {
		return
	}
	var req vmSnapshotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Tag == "" {
		writeError(w, http.StatusBadRequest, "tag is required")
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
		s.Log.Info("uiapi vm restore-snapshot requested", "username", username, "namespace", namespace, "name", name, "tag", req.Tag)
	}
	var out vmSnapshotResponse
	nodePath := "vm-restore-snapshot/" + m.Status.RuntimeID
	if err := s.relayToNode(ctx, nodeAddr, nodePath, req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
