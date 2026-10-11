// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"net/http"

	"github.com/zyvorai/kairon/internal/model"
)

// setPriorityRequest is handleSetMachinePriority's own request body --
// deliberately just the one field, mirroring cmdEditMachine's own
// "priority is the one Machine-spec field this project's edit verb
// supports patching after creation" posture (see kaironctl's own
// cmdEditMachine doc comment and docs/guides/machine-placement.md).
type setPriorityRequest struct {
	Priority int32 `json:"priority"`
}

// createMachineRequest mirrors cmd/kaironctl's cmdCreate flags exactly
// (image/cpu/memory/backend/network/netns/forward/hostname/user/ssh-key/
// package/runcmd), not the full MachineSpec -- the UI's create form offers
// the same surface kaironctl create does, not every field a Machine
// manifest could theoretically set.
type createMachineRequest struct {
	Name              string              `json:"name"`
	CPU               string              `json:"cpu"`
	Memory            string              `json:"memory"`
	Image             string              `json:"image"`
	Backend           string              `json:"backend"`
	Network           string              `json:"network"`
	NetNS             bool                `json:"netns"`
	Forwards          []model.PortForward `json:"forwards,omitempty"`
	Hostname          string              `json:"hostname,omitempty"`
	GuestUser         string              `json:"guestUser,omitempty"`
	SSHAuthorizedKeys []string            `json:"sshAuthorizedKeys,omitempty"`
	Packages          []string            `json:"packages,omitempty"`
	RunCmd            []string            `json:"runCmd,omitempty"`
}

func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request) {
	items, err := s.Kube.ListMachinesNamespace(r.Context(), namespaceParam(r))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// handleListAllMachines serves GET /api/v1/machines?namespace=_all: every
// Machine in every namespace the caller is authorized for. With namespace
// scoping on, a caller without a per-operator identity is refused (the same
// rule requireNamespace applies) and everyone else sees only the namespaces
// authorizedForNamespace grants -- an admin identity sees all of them.
func (s *Server) handleListAllMachines(w http.ResponseWriter, r *http.Request) {
	if !s.allNamespacesAllowed(w, r) {
		return
	}
	items, err := s.Kube.ListMachines(r.Context())
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	visible := make([]model.Machine, 0, len(items))
	for _, m := range items {
		if s.authorizedForNamespace(r.Context(), m.Namespace()) {
			visible = append(visible, m)
		}
	}
	writeJSON(w, http.StatusOK, visible)
}

func (s *Server) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	m, err := s.Kube.GetMachine(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleCreateMachine(w http.ResponseWriter, r *http.Request) {
	var req createMachineRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" || req.Image == "" {
		writeError(w, http.StatusBadRequest, "name and image are required")
		return
	}
	if req.CPU == "" {
		req.CPU = "2"
	}
	if req.Memory == "" {
		req.Memory = "2Gi"
	}
	if req.Backend == "" {
		req.Backend = "qemu"
	}
	if req.Network == "" {
		req.Network = "user"
	}
	ns := namespaceParam(r)
	m := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: req.Name, Namespace: ns},
		Spec: model.MachineSpec{
			Image:     model.ImageSpec{Path: req.Image},
			Resources: model.ResourceSpec{CPU: req.CPU, Memory: req.Memory},
			Runtime:   model.RuntimeSpec{Backend: req.Backend},
			Network:   model.NetworkSpec{Mode: req.Network, NetNS: req.NetNS, Forwards: req.Forwards},
			CloudInit: model.CloudInitSpec{
				Hostname:          req.Hostname,
				User:              req.GuestUser,
				SSHAuthorizedKeys: req.SSHAuthorizedKeys,
				Packages:          req.Packages,
				RunCmd:            req.RunCmd,
			},
			PowerState: "Running",
		},
	}
	out, err := s.Kube.CreateMachine(r.Context(), ns, m)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleDeleteMachine(w http.ResponseWriter, r *http.Request) {
	if err := s.Kube.DeleteMachine(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetMachinePriority patches spec.priority via the exact same
// single-field merge-patch pattern handlePowerMachine already uses for
// spec.powerState -- the dashboard's own counterpart to
// `kaironctl edit machine NAME --priority N`
// (docs/guides/machine-placement.md's "Scheduling priority" section),
// which until now was kaironctl/kubectl-only. Any int32 is accepted,
// including negative or zero (the field's own default) -- there's no
// fixed range for priority, same posture kaironctl's own cmdEditMachine
// already takes. Reuses the ClusterRole's existing "patch" grant on
// machines (already required for power actions); no new RBAC.
func (s *Server) handleSetMachinePriority(w http.ResponseWriter, r *http.Request) {
	var req setPriorityRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	patch := map[string]any{"spec": map[string]any{"priority": req.Priority}}
	if err := s.Kube.PatchMachine(r.Context(), r.PathValue("namespace"), r.PathValue("name"), patch); err != nil {
		writeUpstreamError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePowerMachine returns a handler for the given desired powerState --
// shared by the start/stop/pause/resume routes, mirroring cmdPower's
// single spec.powerState merge-patch (internal/kube.Client.PatchMachine).
func (s *Server) handlePowerMachine(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		patch := map[string]any{"spec": map[string]any{"powerState": state}}
		if err := s.Kube.PatchMachine(r.Context(), r.PathValue("namespace"), r.PathValue("name"), patch); err != nil {
			writeUpstreamError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
