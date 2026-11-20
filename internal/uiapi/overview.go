// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package uiapi

import "net/http"

type overviewResponse struct {
	Machines struct {
		Total   int            `json:"total"`
		ByPhase map[string]int `json:"byPhase"`
	} `json:"machines"`
	Migrations struct {
		Total         int            `json:"total"`
		Active        int            `json:"active"`
		NeedsRecovery int            `json:"needsRecovery"`
		ByPhase       map[string]int `json:"byPhase"`
	} `json:"migrations"`
	Nodes int `json:"nodes"`
}

// isNonTerminalMigrationPhase deliberately duplicates (rather than
// imports) internal/controller's own isActiveMigrationPhase: that
// function is an unexported admission-control implementation detail of
// the controller's concurrency quota (Part II of this work), and this
// dashboard tile has a different, looser purpose -- "is this worth an
// operator's attention" (Pending counts too: a migration that hasn't
// been admitted yet is still work in flight from an operator's point of
// view) rather than "is this consuming node resources right now".
// Coupling a UI aggregation view to the controller's internal admission
// logic would be the wrong dependency direction.
func isNonTerminalMigrationPhase(phase string) bool {
	switch phase {
	case "Succeeded", "Failed", "Blocked", "Cancelled", "":
		return false
	}
	return true
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	machines, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	migrations, err := s.Kube.ListMachineMigrations(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	nodes, err := s.Kube.ListNodes(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	var out overviewResponse
	visibleMachines := machines[:0]
	for _, m := range machines {
		if s.authorizedForNamespace(r.Context(), m.Namespace()) {
			visibleMachines = append(visibleMachines, m)
		}
	}
	machines = visibleMachines
	visibleMigrations := migrations[:0]
	for _, m := range migrations {
		if s.authorizedForNamespace(r.Context(), m.Namespace()) {
			visibleMigrations = append(visibleMigrations, m)
		}
	}
	migrations = visibleMigrations
	out.Machines.Total = len(machines)
	out.Machines.ByPhase = map[string]int{}
	for _, m := range machines {
		phase := m.Status.Phase
		if phase == "" {
			phase = "Unknown"
		}
		out.Machines.ByPhase[phase]++
	}

	out.Migrations.Total = len(migrations)
	out.Migrations.ByPhase = map[string]int{}
	for _, mig := range migrations {
		phase := mig.Status.Phase
		if phase == "" {
			phase = "Pending"
		}
		out.Migrations.ByPhase[phase]++
		if isNonTerminalMigrationPhase(phase) {
			out.Migrations.Active++
		}
		if phase == "NeedsRecovery" {
			out.Migrations.NeedsRecovery++
		}
	}

	if !s.NamespaceScopingEnabled || s.isAdminIdentity(r.Context(), usernameFromContext(r.Context())) {
		out.Nodes = len(nodes)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Kube.ListNodes(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}
