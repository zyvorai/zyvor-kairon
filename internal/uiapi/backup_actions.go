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
	out, err := s.Kube.CreateMachineBackup(r.Context(), ns, model.MachineBackup{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineBackup},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec:     model.MachineBackupSpec{MachineName: machine, Quiesce: req.Quiesce},
	})
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
