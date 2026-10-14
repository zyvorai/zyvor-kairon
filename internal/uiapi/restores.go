// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// createRestoreRequest mirrors cmdRestore's own flags exactly (see
// internal/kaironctl/kaironctl.go's cmdRestore) -- kaironctl restore has
// had full create/get/describe/delete support for MachineSnapshotRestore
// since that CRD shipped; the dashboard had none of it (no route, no
// page) until now, unlike its Snapshot counterpart just above this file.
type createRestoreRequest struct {
	Name             string `json:"name"`
	SnapshotName     string `json:"snapshotName"`
	VolumeName       string `json:"volumeName"`
	TargetClaimName  string `json:"targetClaimName"`
	StorageClassName string `json:"storageClassName"`
	StorageSize      string `json:"storageSize"`
	// Machine also creates a Machine booting from the restored claim.
	Machine *model.RestoreMachineSpec `json:"machine,omitempty"`
}

func (s *Server) handleListRestores(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachineSnapshotRestoresNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleCreateRestore(w http.ResponseWriter, r *http.Request) {
	var req createRestoreRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SnapshotName == "" {
		writeError(w, http.StatusBadRequest, "snapshotName is required")
		return
	}
	if req.TargetClaimName == "" {
		writeError(w, http.StatusBadRequest, "targetClaimName is required")
		return
	}
	ns := namespaceParam(r)
	if req.Name == "" {
		req.Name = resourceName(req.SnapshotName + "-restore-" + time.Now().UTC().Format("20060102-150405"))
	}
	restore := model.MachineSnapshotRestore{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshotRestore},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec: model.MachineSnapshotRestoreSpec{
			SnapshotName:     req.SnapshotName,
			VolumeName:       req.VolumeName,
			TargetClaimName:  req.TargetClaimName,
			StorageClassName: req.StorageClassName,
			StorageSize:      req.StorageSize,
			Machine:          req.Machine,
		},
	}
	out, err := s.Kube.CreateMachineSnapshotRestore(r.Context(), ns, restore)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleDeleteRestore mirrors handleDeleteMachineSet's shape exactly -- a
// plain apiserver delete, no separate admin gate. Unlike MachineSnapshot
// (whose internal/kube.Client has no Delete method at all, see
// createSnapshotRequest's own comment), MachineSnapshotRestore has always
// had one (kaironctl delete restore uses it already), so the dashboard
// gets delete too rather than settling for list+create only.
//
// Deleting a MachineSnapshotRestore never touches the PersistentVolumeClaim
// it already created: status.restoredClaimName's PVC is a normal,
// independent Kubernetes object once restore reaches Succeeded (see
// model.MachineSnapshotRestore's own doc comment) -- this only removes the
// bookkeeping object, exactly like `kubectl delete` on any completed Job
// leaves what the Job produced untouched.
func (s *Server) handleDeleteRestore(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachineSnapshotRestore(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
