// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import "net/http"

// suspendScheduleRequest is the body handleSuspendMachineSnapshotSchedule
// expects -- a single boolean, mirroring `kaironctl edit snapshotschedule
// --suspend`'s own one-field-at-a-time patch shape rather than accepting an
// arbitrary spec patch from the dashboard.
type suspendScheduleRequest struct {
	Suspend bool `json:"suspend"`
}

// scaleMachineSetRequest is the body handleScaleMachineSet expects -- a
// single integer, mirroring `kaironctl scale machineset NAME --replicas
// N`'s own one-field patch shape rather than accepting an arbitrary spec
// patch from the dashboard.
type scaleMachineSetRequest struct {
	Replicas int `json:"replicas"`
}

// This file wraps five CRDs that kaironctl already has full get/describe/
// delete support for (see internal/kaironctl/kaironctl.go), but that
// kairon-ui's own REST API had no route for at all until now: an operator
// using only the dashboard/API -- not kubectl or kaironctl -- had no way
// to see MachineQuota/MachineDisruptionBudget usage (both first-class
// admission-time concerns, see README.md's "Guarding the fleet" section)
// or MachineSet/MachineInstanceType/MigrationPolicy state. Read-only,
// any-authenticated-operator (matching every other read-only
// cross-checkable resource this project exposes, e.g. runtime
// diagnostics/network observability) for four of the five.
//
// MachineSet is the one exception: it now also has DELETE and PATCH
// .../scale routes (handleDeleteMachineSet and handleScaleMachineSet,
// below), matching handleDeleteMachine's own any-authenticated-operator
// gate exactly (no separate admin check -- deleting or scaling a
// MachineSet is no more privileged than deleting a Machine directly,
// which any operator can already do). MachineSet was picked over the
// other four because it's the one an operator manages as a day-to-day
// fleet-sizing operation (create/scale/edit all landed via kaironctl this
// same session) rather than a GitOps-managed policy object
// (MachineQuota/MachineDisruptionBudget/MigrationPolicy) or a
// mostly-static reference value (MachineInstanceType) -- deleting a
// MachineSet now cascades to delete every Machine it owns too (see
// model.FinalizerMachineSet / reconcileMachineSetDeletion in
// internal/controller/machineset.go), so this DELETE route is a real,
// consequential fleet-wide action, not merely "stop managing replicas
// going forward" -- the dashboard's confirm-before-delete UX for this row
// matters more than it once did. Scaling is the other
// routine fleet-sizing action an operator reaches for constantly (unlike
// a full spec edit, which still needs kaironctl/kubectl) -- see
// handleScaleMachineSet's own comment for why replicas is the one field
// worth a dedicated dashboard control. The other four CRDs stay
// list-only, matching this first cut's narrow scope; kaironctl and
// kubectl remain the way to mutate them.

func (s *Server) handleListQuotas(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineQuotasNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListBudgets(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineDisruptionBudgetsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListMachineSets(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineSetsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// handleDeleteMachineSet mirrors handleDeleteMachine's shape exactly
// (internal/uiapi/machines.go) -- a plain apiserver delete, no separate
// admin gate, no cascade cleanup of Machines the MachineSet already
// created (see the file-level comment above for why that's correct).
func (s *Server) handleDeleteMachineSet(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachineSet(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleScaleMachineSet mirrors handleSuspendMachineSnapshotSchedule's
// shape exactly: decode a single-field request, patch just that field via
// a merge-patch (reusing kube.Client.PatchMachineSet, the same method
// `kaironctl scale machineset` already uses), then re-GET and return the
// updated object so the dashboard's table refreshes without a second
// round-trip. Negative replica counts are rejected here rather than left
// to the apiserver: the MachineSet CRD schema already enforces
// `minimum: 0` (charts/kairon/crds/*machineset*.yaml), but failing fast
// with a clear 400 beats forwarding a request the apiserver will reject
// anyway with a less operator-friendly admission error.
func (s *Server) handleScaleMachineSet(w http.ResponseWriter, r *http.Request) {
	var req scaleMachineSetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Replicas < 0 {
		writeError(w, http.StatusBadRequest, "replicas must be >= 0")
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	patch := map[string]any{"spec": map[string]any{"replicas": req.Replicas}}
	if err := s.Kube.PatchMachineSet(r.Context(), namespace, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	updated, err := s.Kube.GetMachineSet(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleListInstanceTypes(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineInstanceTypesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListPreferences(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachinePreferencesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListClusterInstanceTypes(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineClusterInstanceTypes(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListClusterPreferences(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineClusterPreferences(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListMigrationPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMigrationPoliciesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// handleListMachineSnapshotSchedules and handleSuspendMachineSnapshotSchedule
// give MachineSnapshotSchedule (added after this file's other five routes)
// its own dashboard visibility -- any-authenticated-operator list, matching
// every other read-only resource here, plus one write action: pausing or
// resuming a schedule without deleting it. Suspend/resume was picked as
// this CRD's one dashboard mutation for the same reason MachineSet got
// delete (see the file-level comment above) -- it's the one action an
// operator reaches for as routine fleet management (pause backups during a
// maintenance window), not a full spec edit; changing selector/interval/
// keepLast still needs kaironctl/kubectl.
func (s *Server) handleListMachineSnapshotSchedules(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineSnapshotSchedulesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleSuspendMachineSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	var req suspendScheduleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	patch := map[string]any{"spec": map[string]any{"suspend": req.Suspend}}
	if err := s.Kube.PatchMachineSnapshotSchedule(r.Context(), namespace, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	updated, err := s.Kube.GetMachineSnapshotSchedule(r.Context(), namespace, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleListNetworkPolicies and handleListSecurityGroups give
// MachineNetworkPolicy/NetworkSecurityGroup the same dashboard visibility
// every other CRD here already has -- read-only, any-authenticated-
// operator, matching this file's own pattern exactly. Until now these two
// (which drive FluxVM's real eBPF/TC enforcement, see
// docs/guides/network-policy.md) had no kairon-ui route at all: an
// operator using only the dashboard could see a Machine's *effective*
// network policy and drop reasons (the existing network-observability
// endpoints in network_observability.go) but had no way to see the
// MachineNetworkPolicy/NetworkSecurityGroup objects that produced it.
// List-only, like four of the five CRDs above -- kaironctl/kubectl remain
// how they get mutated.
func (s *Server) handleListNetworkPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineNetworkPoliciesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleListSecurityGroups(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListNetworkSecurityGroupsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}
