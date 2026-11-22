// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import "net/http"

// Read-only list routes for the Pools & claims and Backups pages. They mirror
// the other dashboard list pages (fleet.go): namespace-scoped through
// requireNamespace, list-only; kaironctl and kubectl remain how these are
// created and changed.

func (s *Server) handleListMachinePools(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachinePoolsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListMachineClaims(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineClaimsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListMachineBackups(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineBackupsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListMachineBackupRestores(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineBackupRestoresNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
