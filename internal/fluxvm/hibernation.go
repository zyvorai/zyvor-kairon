// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

// HostHibernationFacts mirrors FluxVM's HostHibernationFacts.
type HostHibernationFacts struct {
	QEMUVersion string   `json:"qemu_version"`
	MachineType string   `json:"machine_type"`
	CPUVendor   string   `json:"cpu_vendor"`
	CPUFlags    []string `json:"cpu_flags"`
	StateDir    string   `json:"state_dir"`
}

// Model converts the facts to their Kubernetes form.
func (f HostHibernationFacts) Model() model.HibernationHostFacts {
	return model.HibernationHostFacts{
		QEMUVersion: f.QEMUVersion,
		MachineType: f.MachineType,
		CPUVendor:   f.CPUVendor,
		CPUFlags:    append([]string(nil), f.CPUFlags...),
		StateDir:    f.StateDir,
	}
}

// HibernationManifest mirrors FluxVM's manifest.json.
type HibernationManifest struct {
	Format        uint32               `json:"format"`
	VMID          string               `json:"vm_id"`
	HibernatedAt  time.Time            `json:"hibernated_at"`
	Node          string               `json:"node"`
	Host          HostHibernationFacts `json:"host"`
	VCPUs         uint32               `json:"vcpus"`
	MemoryMiB     uint64               `json:"memory_mib"`
	StateBytes    uint64               `json:"state_bytes"`
	SharedStorage bool                 `json:"shared_storage"`
}

// Hibernation is a VmRecord's hibernation field.
type Hibernation struct {
	Dir      string              `json:"dir"`
	Manifest HibernationManifest `json:"manifest"`
}

func (c *Client) recordCall(ctx context.Context, method, path string, body any, what string) (*Record, error) {
	data, err := c.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", what, err)
	}
	return &rec, nil
}

// Hibernate saves the VM's RAM, CPU and device state and stops QEMU.
// Start resumes it.
func (c *Client) Hibernate(ctx context.Context, id string) (*Record, error) {
	return c.recordCall(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/hibernate", nil, "hibernate")
}

// DiscardHibernation drops the saved state; the next Start boots from disk.
func (c *Client) DiscardHibernation(ctx context.Context, id string) (*Record, error) {
	return c.recordCall(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(id)+"/hibernation", nil, "discard hibernation")
}

// ImportHibernated registers, on this node, a VM another node hibernated
// into the shared hibernation directory. The record keeps id.
func (c *Client) ImportHibernated(ctx context.Context, id string) (*Record, error) {
	return c.recordCall(ctx, http.MethodPost, "/v1/vms/import-hibernated", map[string]any{"id": id}, "import hibernated")
}

// ListVMs returns every VM record on this FluxVM.
func (c *Client) ListVMs(ctx context.Context) ([]Record, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms", nil)
	if err != nil {
		return nil, err
	}
	var arr []Record
	if json.Unmarshal(data, &arr) == nil {
		return arr, nil
	}
	var wrapper struct {
		Items []Record `json:"items"`
		VMs   []Record `json:"vms"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("decode FluxVM VM list: %w", err)
	}
	if len(wrapper.Items) > 0 {
		return wrapper.Items, nil
	}
	return wrapper.VMs, nil
}

// GetHostHibernation reads the facts a saved state depends on.
func (c *Client) GetHostHibernation(ctx context.Context) (*HostHibernationFacts, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/host/hibernation", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Facts HostHibernationFacts `json:"facts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM host hibernation: %w", err)
	}
	return &out.Facts, nil
}
