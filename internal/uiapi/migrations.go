// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/controller"
	"github.com/zyvorai/kairon/internal/model"
)

// createMigrationRequest mirrors cmdMigrate's flags exactly, including
// --migration-network. There is deliberately no delete route --
// internal/kube.Client has none (MachineMigrations are an append-only
// audit trail by design; see docs/architecture.md), and this backend must
// not invent one just because a REST API conventionally has one.
type createMigrationRequest struct {
	Name             string `json:"name"`
	Machine          string `json:"machine"`
	Strategy         string `json:"strategy"`
	TargetNode       string `json:"targetNode"`
	Mode             string `json:"mode"`
	BandwidthMbps    uint64 `json:"bandwidthMbps"`
	MaxDowntimeMs    uint64 `json:"maxDowntimeMs"`
	MultifdChannels  uint8  `json:"multifdChannels"` // decoding already rejects >255 (json.UnmarshalTypeError), unlike cmdMigrate's uint flag
	MigrationNetwork string `json:"migrationNetwork"`
	// Adaptive overrides the matching MigrationPolicy's adaptive settings.
	// Strategy "adaptive" is shorthand for a live migration with an empty
	// adaptive block (policy values or engine defaults).
	Adaptive *model.MachineMigrationAdaptiveSpec `json:"adaptive"`
}

type evacuateRequest struct {
	Node     string `json:"node"`
	Strategy string `json:"strategy"`
}

type evacuateResponse struct {
	Created []string `json:"created"`
}

// recoverRequest mirrors cmdRecover's required --action/--diagnosis/--reason
// triple exactly -- internal/agent's reconcileNeedsRecovery is the only
// thing that actually validates and applies it; this handler is a thin
// spec.recovery patch, same as the CLI.
type recoverRequest struct {
	Action                string `json:"action"`
	AcknowledgedDiagnosis string `json:"acknowledgedDiagnosis"`
	Reason                string `json:"reason"`
}

func (s *Server) handleListMigrations(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineMigrationsNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// handleListAllMigrations serves GET /api/v1/migrations?namespace=_all: every
// MachineMigration in the namespaces the caller is authorized for.
func (s *Server) handleListAllMigrations(w http.ResponseWriter, r *http.Request) {
	if !s.allNamespacesAllowed(w, r) {
		return
	}
	items, err := s.Kube.ListMachineMigrations(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	visible := make([]model.MachineMigration, 0, len(items))
	for _, m := range items {
		if s.authorizedForNamespace(r.Context(), m.Metadata.Namespace) {
			visible = append(visible, m)
		}
	}
	writeJSON(w, http.StatusOK, visible)
}

func (s *Server) handleGetMigration(w http.ResponseWriter, r *http.Request) {
	m, err := s.Kube.GetMachineMigration(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleCreateMigration(w http.ResponseWriter, r *http.Request) {
	var req createMigrationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Machine == "" {
		writeError(w, http.StatusBadRequest, "machine is required")
		return
	}
	if req.Strategy == "" {
		req.Strategy = "auto"
	}
	if req.Mode == "" {
		req.Mode = "pre-copy"
	}
	if req.Strategy == model.MigrationTuningAdaptive {
		req.Strategy = "live"
		if req.Adaptive == nil {
			req.Adaptive = &model.MachineMigrationAdaptiveSpec{}
		}
	}
	if req.Adaptive != nil {
		if req.Strategy != "live" {
			writeError(w, http.StatusBadRequest, "adaptive settings need strategy live or adaptive")
			return
		}
		if err := controller.ValidateAdaptiveSpec(*req.Adaptive); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if c := req.Adaptive.Compression; c != nil && c.Algorithm != model.MigrationCompressionNone && req.MultifdChannels == 0 {
			req.MultifdChannels = 2
		}
	}
	ns := namespaceParam(r)
	if req.Name == "" {
		req.Name = resourceName("migration-" + req.Machine + "-" + time.Now().UTC().Format("20060102-150405"))
	}
	migration := model.MachineMigration{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec: model.MachineMigrationSpec{
			MachineName:      req.Machine,
			Strategy:         req.Strategy,
			TargetNode:       req.TargetNode,
			Mode:             req.Mode,
			BandwidthMbps:    req.BandwidthMbps,
			MaxDowntimeMs:    req.MaxDowntimeMs,
			MultifdChannels:  req.MultifdChannels,
			MigrationNetwork: req.MigrationNetwork,
			Adaptive:         req.Adaptive,
		},
	}
	out, err := s.Kube.CreateMachineMigration(r.Context(), ns, migration)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleEvacuate mirrors cmdEvacuate: bulk-creates one live/cold migration
// per Machine currently assigned to Node, best-effort (a single failed
// create aborts the whole batch and reports what succeeded so far, same
// as the CLI stopping on its first error).
func (s *Server) handleEvacuate(w http.ResponseWriter, r *http.Request) {
	var req evacuateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Node == "" {
		writeError(w, http.StatusBadRequest, "node is required")
		return
	}
	if req.Strategy == "" {
		req.Strategy = "cold"
	}
	if req.Strategy != "cold" && req.Strategy != "auto" {
		writeError(w, http.StatusBadRequest, "strategy must be cold or auto; use /api/v1/migrations for explicit live migration")
		return
	}
	machines, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	// Authorize the entire batch before making its first write.
	for _, machine := range machines {
		if machine.Spec.NodeName == req.Node && machine.Metadata.DeletionTimestamp == nil && !s.authorizedForNamespace(r.Context(), machine.Namespace()) {
			writeError(w, http.StatusForbidden, "evacuation includes an unauthorized namespace")
			return
		}
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	created := make([]string, 0, len(machines))
	for _, machine := range machines {
		if machine.Spec.NodeName != req.Node || machine.Metadata.DeletionTimestamp != nil {
			continue
		}
		name := resourceName("evacuate-" + req.Node + "-" + machine.Metadata.Name + "-" + stamp)
		migration := model.MachineMigration{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration},
			Metadata: model.ObjectMeta{Name: name, Namespace: machine.Namespace()},
			Spec:     model.MachineMigrationSpec{MachineName: machine.Metadata.Name, Strategy: req.Strategy},
		}
		if _, err := s.Kube.CreateMachineMigration(r.Context(), machine.Namespace(), migration); err != nil {
			writeJSON(w, http.StatusPartialContent, map[string]any{
				"created": created,
				"error":   "create migration for " + machine.Namespace() + "/" + machine.Metadata.Name + ": " + err.Error(),
			})
			return
		}
		created = append(created, name)
	}
	writeJSON(w, http.StatusOK, evacuateResponse{Created: created})
}

// handleRecoverMigration mirrors cmdRecover's validation exactly (all
// three fields required together) before ever touching the API server --
// reconcileNeedsRecovery re-validates independently and is the only thing
// that actually applies it; this is a courtesy check, not the safety gate.
func (s *Server) handleRecoverMigration(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	var req recoverRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Action == "" || req.AcknowledgedDiagnosis == "" || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "action, acknowledgedDiagnosis and reason are all required")
		return
	}
	current, err := s.Kube.GetMachineMigration(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if current.Status.Phase != "NeedsRecovery" {
		writeError(w, http.StatusConflict, "machinemigration is in phase "+current.Status.Phase+", not NeedsRecovery")
		return
	}
	patch := map[string]any{
		"spec": map[string]any{
			"recovery": model.MachineMigrationRecoverySpec{
				Action:                req.Action,
				AcknowledgedDiagnosis: req.AcknowledgedDiagnosis,
				Reason:                req.Reason,
			},
		},
	}
	if err := s.Kube.PatchMachineMigration(r.Context(), ns, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCancelMigration mirrors cmdCancelMigration's own local guard --
// cancel only makes sense for a live migration still in Starting or
// Running, before the destination has committed -- so this rejects with a
// clear 409 rather than silently patching spec.cancel onto a migration the
// source agent will just ignore it on. internal/agent's reconcileMigration
// is what actually validates and applies it (this handler only ever sets
// spec.cancel; it never touches status directly), same "thin patch, real
// node agent owns the effect" shape as handleRecoverMigration.
func (s *Server) handleCancelMigration(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	current, err := s.Kube.GetMachineMigration(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if current.Status.Phase != "Starting" && current.Status.Phase != "Running" {
		writeError(w, http.StatusConflict, "machinemigration is in phase "+current.Status.Phase+"; cancel only applies to a live migration still in Starting or Running, before the destination has committed")
		return
	}
	if current.Status.EffectiveStrategy != "live" {
		writeError(w, http.StatusConflict, "machinemigration is a "+current.Status.EffectiveStrategy+"-strategy migration; cancel only applies to live migrations")
		return
	}
	patch := map[string]any{"spec": map[string]any{"cancel": true}}
	if err := s.Kube.PatchMachineMigration(r.Context(), ns, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
