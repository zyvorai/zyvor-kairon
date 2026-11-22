// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package uiapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// requireCatalogAdmin (internal/uiapi/catalog.go) is reused as-is for
// pools too -- the same "mutating node-scoped FluxVM state is admin-only,
// the deployment needs the console relay configured" posture applies
// verbatim; a pool's own template embeds a full CreateRequest, at least
// as sensitive to let an arbitrary operator mutate as a catalog entry.

// poolTimeout is generous, matching catalogTimeout: creating a pool
// triggers FluxVM's own background backfill (booting spec.size VMs), and
// while the create call itself returns immediately (the backfill is
// async), a claim can involve a real VM resume.
const poolTimeout = execRelayClientTimeout

// handleListPools lists every warm-VM pool on a node: kairon-ui ->
// kairon-node (internal/consoleproxy) -> FluxVM's own GET /v1/pools. Any
// authenticated operator -- read-only visibility, same posture as
// sandboxes/templates/catalog listing.
func (s *Server) handleListPools(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "pools are not enabled on this deployment")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "pools", &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetPool returns one pool's current state: kairon-ui -> kairon-node
// -> FluxVM's own GET /v1/pools/{name}. Same any-operator posture as
// handleListPools.
func (s *Server) handleGetPool(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	if s.ConsoleToken == "" || s.ConsolePort == "" {
		writeError(w, http.StatusNotImplemented, "pools are not enabled on this deployment")
		return
	}
	nodeAddr, err := s.nodeInternalIP(r.Context(), nodeName)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	var out map[string]any
	if err := s.relayToNodeGet(ctx, nodeAddr, "pools/"+name, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createPoolRequest mirrors fluxvm.PoolSpec's own JSON shape -- a
// separate wire type from Kairon's own Machine spec, since a pool's
// template is FluxVM's raw CreateVmRequest shape, not a Kubernetes
// Machine spec.
type createPoolRequest struct {
	Name     string         `json:"name"`
	Size     int            `json:"size"`
	Template map[string]any `json:"template"`
}

// handleCreatePool creates a new warm-VM pool on a node: kairon-ui ->
// kairon-node -> FluxVM's own POST /v1/pools. Admin-only, the same
// posture handleAddCatalogEntry/handleBuildTemplate already have --
// booting spec.size VMs ahead of time is a real, ongoing resource
// commitment on the node.
func (s *Server) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	nodeName := r.PathValue("node")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req createPoolRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), poolTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi pool create requested", "username", usernameFromContext(r.Context()), "node", nodeName, "name", req.Name, "size", req.Size)
	}
	var out map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "pools", req, &out); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeletePool deletes a pool and every one of its member VMs:
// kairon-ui -> kairon-node -> FluxVM's own DELETE /v1/pools/{name}.
// Admin-only, and genuinely destructive -- every member VM the pool
// currently holds is deleted too, claimed or not.
func (s *Server) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), execRelayClientTimeout)
	defer cancel()
	if s.Log != nil {
		s.Log.Info("uiapi pool delete requested", "username", usernameFromContext(r.Context()), "node", nodeName, "name", name)
	}
	if err := s.relayToNodeDelete(ctx, nodeAddr, "pools/"+name); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type claimPoolRequest struct {
	Name       string `json:"name,omitempty"`
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
	// CreateMachine, when true, additionally creates a real Kubernetes
	// Machine object for the claimed VM once the claim itself succeeds --
	// closing the "claimed VM sits outside MachineQuota/the admission
	// webhook until an operator explicitly creates one" gap README.md's
	// own Production gaps section names. Opt-in and off by default: a
	// request that omits it gets exactly the prior behavior, byte for
	// byte -- warm pools exist for fast, low-ceremony ephemeral VMs (see
	// docs/guides/machine-sandboxes.md), and a full Machine object drags
	// in finalizer-gated deletion, quota accounting, webhook validation,
	// and dashboard visibility that not every caller wants.
	//
	// Requires MachineName (fails closed with 400 rather than guessing a
	// name). Namespace defaults to "default" like everywhere else in this
	// API. The claimed VM's own FluxVM-side name is forced to
	// Machine{Name: MachineName, Namespace: Namespace}.RuntimeName() --
	// overriding any Name set above -- so kairon-node's existing adoption
	// path (Agent.current falling back to LookupByName when
	// status.runtimeID is unset) picks up this exact runtime on its next
	// reconcile tick instead of creating a second, duplicate VM.
	CreateMachine bool   `json:"createMachine,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	MachineName   string `json:"machineName,omitempty"`
}

// machineSpecFromPoolTemplate builds a model.MachineSpec from a pool's own
// Template (FluxVM's raw CreateRequest shape, stored as map[string]any
// since Kairon never otherwise needs to understand a pool's template
// contents -- see createPoolRequest's own doc comment). Every field with a
// Machine spec equivalent is carried, so a later recreate of the Machine
// boots the same VM the pool did. Fields that have no safe equivalent
// come back as warnings instead of being dropped silently: VFIO devices
// (one device can back only one VM), the raw network map (per-VM MACs),
// shared folders, data disks and a storage override.
func machineSpecFromPoolTemplate(template map[string]any) (model.MachineSpec, []string, error) {
	raw, err := json.Marshal(template)
	if err != nil {
		return model.MachineSpec{}, nil, err
	}
	var cr fluxvm.CreateRequest
	if err := json.Unmarshal(raw, &cr); err != nil {
		return model.MachineSpec{}, nil, err
	}
	backend := cr.Backend
	if backend == "" {
		backend = "qemu"
	}
	memory := "2Gi"
	if cr.MemoryMiB > 0 {
		memory = strconv.FormatUint(cr.MemoryMiB, 10) + "Mi"
	}
	cpu := "2"
	if cr.VCPUs > 0 {
		cpu = strconv.FormatUint(uint64(cr.VCPUs), 10)
	}
	spec := model.MachineSpec{
		Image: model.ImageSpec{Path: cr.Image},
		Resources: model.ResourceSpec{
			CPU: cpu, Memory: memory,
			Hugepages: cr.Hugepages, NUMANode: cr.NUMANode, CPUSet: cr.CPUSet,
		},
		Runtime:    model.RuntimeSpec{Backend: backend, Kernel: cr.Kernel},
		PowerState: "Running",
		TTLSeconds: cr.TTLSeconds,
		Security:   model.SecuritySpec{SecureBoot: cr.SecureBoot, TPM: cr.TPM},
		GuestAgent: model.GuestAgentSpec{
			Enabled: cr.Qga != nil && cr.Qga.Enabled,
			Console: cr.Agent != nil && cr.Agent.Enabled,
		},
	}
	if cr.MaxVCPUs != nil {
		spec.Resources.MaxCPU = strconv.FormatUint(uint64(*cr.MaxVCPUs), 10)
	}
	if cr.MaxMemoryMiB != nil {
		spec.Resources.MaxMemory = strconv.FormatUint(*cr.MaxMemoryMiB, 10) + "Mi"
	}
	if ci := cr.CloudInit; ci != nil {
		spec.CloudInit = model.CloudInitSpec{
			Hostname: ci.Hostname, User: ci.User, SSHAuthorizedKeys: ci.SSHAuthorizedKeys,
			Packages: ci.Packages, RunCmd: ci.RunCmd,
		}
		for _, f := range ci.WriteFiles {
			spec.CloudInit.WriteFiles = append(spec.CloudInit.WriteFiles, model.CloudInitFile{Path: f.Path, Content: f.Content, Permissions: f.Permissions})
		}
		spec.Network.StaticNetwork = ci.StaticNetwork
	}
	var warnings []string
	if len(cr.VFIODevices) > 0 {
		warnings = append(warnings, "template vfio_devices not carried: a device can back only one VM; use spec.deviceClaims on the Machine if it needs one")
	}
	if forwards, ok := userModeForwards(cr.Network); ok {
		spec.Network.Forwards = forwards
	} else {
		warnings = append(warnings, "template network not carried: set spec.network on the Machine if it needs more than the default NIC")
	}
	if len(cr.SharedFolders) > 0 {
		warnings = append(warnings, "template shared_folders not carried: use spec.volumes on the Machine")
	}
	if len(cr.DataDisks) > 0 {
		warnings = append(warnings, "template data_disks not carried: use spec.disks on the Machine")
	}
	if cr.Storage != "" && cr.Storage != "default" {
		warnings = append(warnings, "template storage "+strconv.Quote(cr.Storage)+" not carried: a recreated Machine boots from its image path")
	}
	return spec, warnings, nil
}

// userModeForwards reads a template network map that a Machine's default
// user-mode NIC can express: empty, or mode "user" with only port
// forwards. FluxVM fills mode/forwards into every stored template, so
// this is the common case. Anything else (tap, bridges, MACs, extra
// NICs) reports ok=false.
func userModeForwards(network map[string]any) ([]model.PortForward, bool) {
	if len(network) == 0 {
		return nil, true
	}
	for key := range network {
		if key != "mode" && key != "forwards" {
			return nil, false
		}
	}
	if mode, _ := network["mode"].(string); mode != "" && mode != "user" {
		return nil, false
	}
	raw, err := json.Marshal(network["forwards"])
	if err != nil {
		return nil, false
	}
	var wire []struct {
		HostPort  uint16 `json:"host_port"`
		GuestPort uint16 `json:"guest_port"`
		Protocol  string `json:"protocol"`
	}
	if string(raw) != "null" {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, false
		}
	}
	var forwards []model.PortForward
	for _, f := range wire {
		forwards = append(forwards, model.PortForward{HostPort: f.HostPort, GuestPort: f.GuestPort, Protocol: f.Protocol})
	}
	return forwards, true
}

// handleClaimPool pops one ready pool member, resumes it, and returns the
// now-Running VM: kairon-ui -> kairon-node -> FluxVM's own
// POST /v1/pools/{name}/claim. Admin-only, same posture as creating a
// pool. The claimed VM is real FluxVM state, not automatically wired into
// a Kairon Machine object unless req.CreateMachine is set -- see
// docs/guides/machine-sandboxes.md's "Warm pools" section for what that
// means in practice either way.
func (s *Server) handleClaimPool(w http.ResponseWriter, r *http.Request) {
	nodeName, name := r.PathValue("node"), r.PathValue("name")
	nodeAddr, ok := s.requireCatalogAdmin(w, r, nodeName)
	if !ok {
		return
	}
	var req claimPoolRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	ns := req.Namespace
	if req.CreateMachine {
		if req.MachineName == "" {
			writeError(w, http.StatusBadRequest, "machineName is required when createMachine is true")
			return
		}
		if ns == "" {
			ns = model.DefaultNamespace
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), poolTimeout)
	defer cancel()

	var template map[string]any
	if req.CreateMachine {
		// Fetch the pool's own template -- what every member, including
		// the one about to be claimed, was actually booted from -- rather
		// than trying to reconstruct a Machine spec from the claimed
		// record's own FluxVM-side fields, which don't carry everything a
		// Machine spec needs (e.g. the image path isn't echoed back on a
		// claim response).
		var poolOut map[string]any
		if err := s.relayToNodeGet(ctx, nodeAddr, "pools/"+name, &poolOut); err != nil {
			writeError(w, http.StatusBadGateway, "fetching pool template: "+err.Error())
			return
		}
		if t, ok := poolOut["template"].(map[string]any); ok {
			template = t
		}
	}

	claimName := req.Name
	if req.CreateMachine {
		// Force the FluxVM-side name so kairon-node's existing adoption
		// path (LookupByName by RuntimeName() when status.runtimeID is
		// unset) picks up this exact runtime on its next reconcile tick,
		// instead of creating a second, duplicate VM for the Machine
		// object below.
		claimName = model.Machine{Metadata: model.ObjectMeta{Name: req.MachineName, Namespace: ns}}.RuntimeName()
	}
	body := struct {
		Name       string `json:"name,omitempty"`
		TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
	}{Name: claimName, TTLSeconds: req.TTLSeconds}
	if s.Log != nil {
		s.Log.Info("uiapi pool claim requested", "username", usernameFromContext(r.Context()), "node", nodeName, "pool", name, "createMachine", req.CreateMachine)
	}
	var claimed map[string]any
	if err := s.relayToNode(ctx, nodeAddr, "pools/"+name+"/claim", body, &claimed); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !req.CreateMachine {
		writeJSON(w, http.StatusOK, claimed)
		return
	}

	spec, warnings, err := machineSpecFromPoolTemplate(template)
	if err != nil {
		// The claim itself already succeeded and the VM is real, running
		// state -- report the Machine-creation failure without pretending
		// the claim didn't happen, so the operator knows to create the
		// Machine themselves (or retry) rather than assume nothing exists.
		writeJSON(w, http.StatusOK, map[string]any{
			"vm": claimed, "machine": nil,
			"machineError": "parsing pool template for Machine spec: " + err.Error(),
		})
		return
	}
	// The claimed VM already runs on this node; without nodeName the
	// scheduler could place the Machine elsewhere and boot a duplicate.
	spec.NodeName = nodeName
	if req.TTLSeconds != nil {
		spec.TTLSeconds = *req.TTLSeconds
	}
	machine := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: req.MachineName, Namespace: ns},
		Spec:     spec,
	}
	created, err := s.Kube.CreateMachine(ctx, ns, machine)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"vm": claimed, "machine": nil,
			"machineError": "the pool claim succeeded but creating the Machine object failed: " + err.Error(),
		})
		return
	}
	out := map[string]any{"vm": claimed, "machine": created}
	if len(warnings) > 0 {
		out["machineWarnings"] = warnings
	}
	writeJSON(w, http.StatusOK, out)
}
