// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// RuntimeMigrationCapability mirrors FluxVM's own struct of the same
// name (fluxvm-core's model module) -- one backend's real, current live
// migration support, not Kairon's own hardcoded assumptions about it.
type RuntimeMigrationCapability struct {
	Backend               string   `json:"backend"`
	Live                  bool     `json:"live"`
	PreCopy               bool     `json:"preCopy"`
	PostCopy              bool     `json:"postCopy"`
	Multifd               bool     `json:"multifd"`
	RequiresSharedStorage bool     `json:"requiresSharedStorage"`
	Transports            []string `json:"transports"`
}

// RuntimeSnapshotCapability mirrors FluxVM's own struct of the same name.
type RuntimeSnapshotCapability struct {
	Backend  string `json:"backend"`
	Memory   bool   `json:"memory"`
	Disk     bool   `json:"disk"`
	Portable bool   `json:"portable"`
}

// RuntimeCapabilities mirrors FluxVM's own RuntimeCapabilities -- a
// static, per-node manifest of what this node's FluxVM actually supports,
// per backend. Kairon's own code today hardcodes several of the same
// facts this reports (e.g. "spec.resources.numaNode/cpuSet/hugepages
// require the qemu backend" in internal/fluxvm.buildCreateRequest,
// "Firecracker doesn't support VM-state snapshot" in
// internal/fluxvm/hibernate.go's own doc comments) -- this endpoint is
// exposed as a diagnostic a caller can cross-check against, not (yet) a
// replacement for those hardcoded checks; see
// docs/guides/machine-diagnostics.md's "Runtime capabilities" section for
// why they weren't simply deleted in favor of always querying this
// instead.
type RuntimeCapabilities struct {
	APIVersion         string                       `json:"apiVersion"`
	Scope              string                       `json:"scope"`
	OrchestrationOwner string                       `json:"orchestrationOwner"`
	Migration          []RuntimeMigrationCapability `json:"migration"`
	Snapshot           []RuntimeSnapshotCapability  `json:"snapshot"`
	Backup             []RuntimeBackupCapability    `json:"backup,omitempty"`
}

// RuntimeBackupCapability is one backup backend FluxVM offers; "cbt" with
// incremental and an "s3" target means S3 incremental backups work here.
type RuntimeBackupCapability struct {
	Backend     string   `json:"backend"`
	Full        bool     `json:"full"`
	Incremental bool     `json:"incremental"`
	Targets     []string `json:"targets"`
	Compression []string `json:"compression"`
	Encryption  []string `json:"encryption"`
}

// CBTBackup reports whether FluxVM takes incremental backups to S3.
func (r *RuntimeCapabilities) CBTBackup() bool {
	if r == nil {
		return false
	}
	for _, b := range r.Backup {
		if b.Incremental {
			for _, t := range b.Targets {
				if t == "s3" {
					return true
				}
			}
		}
	}
	return false
}

// GetRuntimeCapabilities returns this node's real, current capability
// manifest -- FluxVM's own GET /v1/runtime/capabilities. No auth
// requirement on FluxVM's own side (static, non-sensitive config).
func (c *Client) GetRuntimeCapabilities(ctx context.Context) (*RuntimeCapabilities, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/runtime/capabilities", nil)
	if err != nil {
		return nil, err
	}
	var out RuntimeCapabilities
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM runtime capabilities: %w", err)
	}
	return &out, nil
}

// PressureRecord mirrors FluxVM's own fluxvm_cgroup::PressureRecord --
// one resource's PSI (Pressure Stall Information) reading, straight from
// the kernel's own /proc/pressure accounting for this VM's cgroup.
type PressureRecord struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  uint64  `json:"total"`
}

// Pressure mirrors FluxVM's own VmPressure -- CPU only ever has "some"
// (a task is stalled on the resource); memory and IO have both "some"
// (at least one task stalled) and "full" (every task stalled). Any field
// may be nil if the kernel didn't have that particular PSI file
// available.
type Pressure struct {
	CPUSome    *PressureRecord `json:"cpu_some"`
	MemorySome *PressureRecord `json:"memory_some"`
	MemoryFull *PressureRecord `json:"memory_full"`
	IOSome     *PressureRecord `json:"io_some"`
	IOFull     *PressureRecord `json:"io_full"`
}

// GetPressure returns a VM's real, cgroup-derived PSI pressure stats --
// FluxVM's own GET /v1/vms/{id}/pressure. Complements GetStats (raw
// usage numbers): pressure reports how often tasks in this VM's cgroup
// are actually *stalled* waiting on a resource, a more direct signal of
// contention than usage percentage alone.
func (c *Client) GetPressure(ctx context.Context, id string) (*Pressure, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/pressure", nil)
	if err != nil {
		return nil, err
	}
	var out Pressure
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode vm pressure: %w", err)
	}
	return &out, nil
}

// GetCPUSet returns the real, effective set of host CPU numbers this
// VM's cgroup is currently allowed to run on -- FluxVM's own
// GET /v1/vms/{id}/cpuset. Read-only visibility into whatever the host
// actually did, not a Kairon-managed allocation: see
// spec.resources.cpuSet's own doc comment (docs/guides/machine-cpu-numa.md)
// for why Kairon doesn't allocate or guarantee non-overlapping host cores
// across competing Machines today -- this just reports ground truth.
func (c *Client) GetCPUSet(ctx context.Context, id string) ([]uint32, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/cpuset", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		CPUs []uint32 `json:"cpus"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode vm cpuset: %w", err)
	}
	return out.CPUs, nil
}

// Freeze halts a VM's entire cgroup (every process in it -- the VMM
// itself and any of its own helper threads) at the kernel scheduler
// level via cgroup v2's freezer controller -- FluxVM's own
// POST /v1/vms/{id}/freeze. A genuinely different mechanism from Pause
// (QMP-level, guest-CPU-only, hypervisor-cooperative) and from QGA
// fsfreeze (guest-filesystem-level, requires the guest's own
// cooperation): this is a pure host-kernel operation that works
// regardless of whether the hypervisor's own control channel (QMP, the
// vsock agent) is responsive at all -- useful as a last-resort freeze
// when diagnosing a hung VM, not a routine pause/resume replacement.
func (c *Client) Freeze(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/freeze", nil)
	return err
}

// Thaw reverses Freeze -- FluxVM's own POST /v1/vms/{id}/thaw.
func (c *Client) Thaw(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/thaw", nil)
	return err
}

// IsFrozen reports whether a VM's cgroup is currently frozen -- FluxVM's
// own GET /v1/vms/{id}/frozen.
func (c *Client) IsFrozen(ctx context.Context, id string) (bool, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/frozen", nil)
	if err != nil {
		return false, err
	}
	var out struct {
		Frozen bool `json:"frozen"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false, fmt.Errorf("decode vm frozen status: %w", err)
	}
	return out.Frozen, nil
}
