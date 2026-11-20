// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"

	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/model"
)

func (s *Server) handleListFleet(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	kind, ok := model.FleetKinds[resource]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown fleet resource")
		return
	}
	if (kind == "MachineHAProfile" || kind == "NodeFenceRequest") && !s.requireAdmin(w, r) {
		return
	}
	objects, err := s.Kube.ListFleet(r.Context(), namespaceParam(r), resource)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if objects == nil {
		objects = []model.FleetResource{}
	}
	writeJSON(w, http.StatusOK, objects)
}
func (s *Server) handleCreateFleet(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	kind, ok := model.FleetKinds[resource]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown fleet resource")
		return
	}
	// Approval creation must use the approver's own Kubernetes credential,
	// rather than the UI server's shared service-account authority.
	if kind == "MachineActionApproval" {
		writeError(w, http.StatusForbidden, "use kaironctl fleet approve-action with a separate Kubernetes approver identity")
		return
	}
	if kind != "MachineTemplateClaim" && kind != "MachineNetworkClaim" && !s.requireAdmin(w, r) {
		return
	}
	var obj model.FleetResource
	if err := decodeJSON(w, r, &obj); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	obj.TypeMeta = model.TypeMeta{APIVersion: model.FleetAPIVersion, Kind: kind}
	obj.Metadata.Namespace = namespaceParam(r)
	obj.Status = model.FleetStatus{}
	if obj.Metadata.UID != "" || obj.Metadata.ResourceVersion != "" || obj.Metadata.DeletionTimestamp != nil {
		writeError(w, http.StatusBadRequest, "server-managed metadata cannot be supplied")
		return
	}
	if err := fleet.Validate(obj); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.Kube.CreateFleet(r.Context(), namespaceParam(r), resource, obj)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}
func (s *Server) handleDeleteFleet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	resource := r.PathValue("resource")
	if _, ok := model.FleetKinds[resource]; !ok {
		writeError(w, http.StatusNotFound, "unknown fleet resource")
		return
	}
	if err := s.Kube.DeleteFleet(r.Context(), r.PathValue("namespace"), resource, r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleLedgerCSV(w http.ResponseWriter, r *http.Request) {
	ledgers, err := s.Kube.ListFleet(r.Context(), namespaceParam(r), "machineusageledgers")
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="kairon-usage.csv"`)
	out := csv.NewWriter(w)
	if err := out.Write([]string{"namespace", "ledger", "vcpu_hours", "memory_gib_hours", "machine_hours", "unobserved_seconds"}); err != nil {
		return
	}
	for _, o := range ledgers {
		row := []string{o.Metadata.Namespace, o.Metadata.Name}
		for _, key := range []string{"vcpu_hours", "memory_gib_hours", "machine_hours", "unobserved_seconds"} {
			row = append(row, strconv.FormatFloat(o.Status.Totals[key], 'f', 6, 64))
		}
		if err := out.Write(row); err != nil {
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil && s.Log != nil {
		s.Log.Warn("usage CSV write failed", "error", fmt.Sprint(err))
	}
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.isAdminIdentity(r.Context(), usernameFromContext(r.Context())) {
		writeError(w, http.StatusForbidden, "administrator identity required")
		return false
	}
	return true
}
