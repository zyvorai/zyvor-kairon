// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

type createBackupRequest struct {
	Name    string `json:"name"`
	Quiesce string `json:"quiesce"` // auto (default), required or never
	// S3 (incremental-capable) backups: mode full, incremental or
	// synthetic-full, with the target taken from policy or from parent.
	Mode   string `json:"mode,omitempty"`
	Parent string `json:"parent,omitempty"`
	Policy string `json:"policy,omitempty"`
}

// handleCreateBackup serves POST /api/v1/machines/{ns}/{name}/backups: the
// same MachineBackup `kaironctl backup create` writes, minus the Atlas
// options (those stay CLI/YAML).
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	ns, machine := r.PathValue("namespace"), r.PathValue("name")
	var req createBackupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !model.ValidBackupQuiesce(req.Quiesce) {
		writeError(w, http.StatusBadRequest, "quiesce must be auto, required or never")
		return
	}
	if req.Name == "" {
		req.Name = resourceName(machine + "-" + time.Now().UTC().Format("20060102-150405"))
	}
	b := model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec:     model.MachineBackupSpec{MachineName: machine, Quiesce: req.Quiesce, Mode: req.Mode, Parent: req.Parent},
	}
	var policy *model.MachineBackupPolicy
	var parent *model.MachineBackup
	if req.Policy != "" {
		p, err := s.Kube.GetMachineBackupPolicy(r.Context(), ns, req.Policy)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		policy = &p
	} else if req.Parent != "" {
		p, err := s.Kube.GetMachineBackup(r.Context(), ns, req.Parent)
		if err != nil {
			writeUpstreamError(w, err)
			return
		}
		parent = &p
	}
	if err := model.InheritBackupTarget(&b, policy, parent); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := model.ValidateCBTBackup(b); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.Kube.CreateMachineBackup(r.Context(), ns, b)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type restoreBackupRequest struct {
	Name         string `json:"name"`
	Machine      string `json:"machine"`      // default: the backed-up Machine, which must be halted
	StorageClass string `json:"storageClass"` // for restored Atlas volumes
}

// handleRestoreBackup serves POST /api/v1/machinebackups/{ns}/{name}/restore.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	ns, backup := r.PathValue("namespace"), r.PathValue("name")
	var req restoreBackupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		req.Name = resourceName(backup + "-restore-" + time.Now().UTC().Format("20060102-150405"))
	}
	out, err := s.Kube.CreateMachineBackupRestore(r.Context(), ns, model.MachineBackupRestore{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupRestore},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec:     model.MachineBackupRestoreSpec{BackupName: backup, MachineName: req.Machine, StorageClassName: req.StorageClass},
	})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleDeleteBackup serves DELETE /api/v1/machinebackups/{ns}/{name}.
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachineBackup(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBackupChains serves GET /api/v1/machines/{ns}/{name}/backup-chains:
// the Machine's S3 backups grouped into chains, for the chain view and
// restore from any point.
func (s *Server) handleBackupChains(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineBackupsNamespace(r.Context(), r.PathValue("namespace"))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	chains := model.BackupChains(items, r.PathValue("name"))
	if chains == nil {
		chains = []model.BackupChainView{}
	}
	writeJSON(w, http.StatusOK, chains)
}

func (s *Server) handleListBackupPolicies(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineBackupPoliciesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

type createBackupPolicyRequest struct {
	Name string                        `json:"name"`
	Spec model.MachineBackupPolicySpec `json:"spec"`
}

// handleCreateBackupPolicy serves POST /api/v1/machinebackuppolicies/{ns}.
func (s *Server) handleCreateBackupPolicy(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	var req createBackupPolicyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := req.Spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.Kube.CreateMachineBackupPolicy(r.Context(), ns, model.MachineBackupPolicy{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackupPolicy},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec:     req.Spec,
	})
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleSuspendBackupPolicy serves POST
// /api/v1/machinebackuppolicies/{ns}/{name}/suspend with {"suspend": bool}.
func (s *Server) handleSuspendBackupPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suspend bool `json:"suspend"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.Kube.PatchMachineBackupPolicy(r.Context(), r.PathValue("namespace"), r.PathValue("name"), map[string]any{"spec": map[string]any{"suspend": req.Suspend}}); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachineBackupPolicy(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
