// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// The handlers below expose actions `kaironctl` already has (fork, disk,
// nic, trigger). They reproduce the same spec edits, validated by the same
// model functions, rather than inventing UI-only behaviour; the controller
// and kairon-node act on the result exactly as they do for the CLI.

const maxForkCount = 32

type forkRequest struct {
	Count  int    `json:"count"`
	Prefix string `json:"prefix"`
}

// handleForkMachine serves POST /api/v1/machines/{ns}/{name}/fork: creates
// Count children that copy the parent's spec, pinned to its node and
// annotated so kairon-node forks the parent's runtime (see kaironctl fork).
func (s *Server) handleForkMachine(w http.ResponseWriter, r *http.Request) {
	ns, parent := r.PathValue("namespace"), r.PathValue("name")
	var req forkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if req.Count < 1 || req.Count > maxForkCount {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("count must be 1-%d", maxForkCount))
		return
	}
	src, err := s.Kube.GetMachine(r.Context(), ns, parent)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if src.Status.Phase != "Running" || src.Spec.NodeName == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("machine %s/%s is %q; only a Running Machine can be forked", ns, parent, src.Status.Phase))
		return
	}
	if len(src.Spec.Volumes) > 0 || len(src.Spec.Disks) > 0 || len(src.Spec.DeviceClaims) > 0 {
		writeError(w, http.StatusConflict, "machine has volumes, disks or device claims; fork copies only the boot disk")
		return
	}
	prefix := req.Prefix
	if prefix == "" {
		buf := make([]byte, 3)
		_, _ = rand.Read(buf)
		prefix = parent + "-fork-" + hex.EncodeToString(buf)
	}
	names := make([]string, req.Count)
	for i := range names {
		names[i] = fmt.Sprintf("%s-%d", prefix, i+1)
		// FluxVM names the child runtime RuntimeName()+"-1", max 63 chars.
		if n := len("kairon-"+ns+"-"+names[i]) + 2; n > 63 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("child name %q is too long for a FluxVM runtime name (%d > 63 chars)", names[i], n))
			return
		}
	}
	spec := src.Spec
	spec.PowerState = "Running"
	created := make([]string, 0, len(names))
	for _, name := range names {
		child := model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{
				Name: name, Namespace: ns,
				Labels:      map[string]string{model.LabelForkedFrom: parent, model.AssignedNodeLabel: src.Spec.NodeName},
				Annotations: map[string]string{model.AnnotationForkFrom: parent},
			},
			Spec: spec,
		}
		if _, err := s.Kube.CreateMachine(r.Context(), ns, child); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("create %s/%s: %v", ns, name, err), "created": created})
			return
		}
		created = append(created, name)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": created})
}

type hotplugRequest struct {
	Action string `json:"action"` // attach|detach (disks), add|remove (nics)
	Name   string `json:"name"`
	Claim  string `json:"claim,omitempty"`  // disks
	Bridge string `json:"bridge,omitempty"` // nics
	MAC    string `json:"mac,omitempty"`    // nics
}

// handleHotplugDisk serves POST /api/v1/machines/{ns}/{name}/disks.
func (s *Server) handleHotplugDisk(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	var req hotplugRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" || (req.Action != "attach" && req.Action != "detach") {
		writeError(w, http.StatusBadRequest, "name and action (attach or detach) are required")
		return
	}
	if req.Action == "attach" && req.Claim == "" {
		writeError(w, http.StatusBadRequest, "claim is required to attach a disk")
		return
	}
	m, err := s.Kube.GetMachine(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	disks := slices.Clone(m.Spec.Disks)
	i := slices.IndexFunc(disks, func(d model.MachineDisk) bool { return d.Name == req.Name })
	switch {
	case req.Action == "attach" && i >= 0:
		writeError(w, http.StatusConflict, fmt.Sprintf("disk %q is already in spec.disks", req.Name))
		return
	case req.Action == "attach":
		disks = append(disks, model.MachineDisk{Name: req.Name, ClaimName: req.Claim})
		if err := model.ValidateDisks(disks); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case i < 0:
		writeError(w, http.StatusConflict, fmt.Sprintf("disk %q is not in spec.disks", req.Name))
		return
	default:
		disks = slices.Delete(disks, i, i+1)
	}
	patch := map[string]any{
		"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
		"spec":     map[string]any{"disks": disks},
	}
	if err := s.Kube.PatchMachine(r.Context(), ns, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHotplugNIC serves POST /api/v1/machines/{ns}/{name}/nics.
func (s *Server) handleHotplugNIC(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	var req hotplugRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" || (req.Action != "add" && req.Action != "remove") {
		writeError(w, http.StatusBadRequest, "name and action (add or remove) are required")
		return
	}
	if req.Action == "add" && req.Bridge == "" {
		writeError(w, http.StatusBadRequest, "bridge is required to add an interface")
		return
	}
	m, err := s.Kube.GetMachine(r.Context(), ns, name)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	spec := m.Spec.Network
	list := slices.Clone(spec.ExtraInterfaces)
	i := slices.IndexFunc(list, func(x model.ExtraInterface) bool { return x.Name == req.Name })
	switch {
	case req.Action == "add" && i >= 0:
		writeError(w, http.StatusConflict, fmt.Sprintf("interface %q is already in spec.network.extraInterfaces", req.Name))
		return
	case req.Action == "add":
		list = append(list, model.ExtraInterface{Name: req.Name, Bridge: req.Bridge, MAC: req.MAC})
		spec.ExtraInterfaces = list
		if err := model.ValidateExtraInterfaces(spec); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case i < 0:
		writeError(w, http.StatusConflict, fmt.Sprintf("interface %q is not in spec.network.extraInterfaces", req.Name))
		return
	default:
		list = slices.Delete(list, i, i+1)
	}
	patch := map[string]any{
		"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
		"spec":     map[string]any{"network": map[string]any{"extraInterfaces": list}},
	}
	if err := s.Kube.PatchMachine(r.Context(), ns, name, patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTriggerSnapshotSchedule serves
// POST /api/v1/snapshot-schedules/{ns}/{name}/trigger: asks the controller
// for an out-of-band run via the same annotation `kaironctl trigger` sets.
func (s *Server) handleTriggerSnapshotSchedule(w http.ResponseWriter, r *http.Request) {
	ts := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationSnapshotScheduleTriggerNow: ts}}}
	if err := s.Kube.PatchMachineSnapshotSchedule(r.Context(), r.PathValue("namespace"), r.PathValue("name"), patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
