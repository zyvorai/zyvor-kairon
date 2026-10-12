// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// HostMdev is FluxVM's GET /v1/host/mdev.
type HostMdev struct {
	Parents   []HostMdevParent   `json:"parents"`
	Instances []HostMdevInstance `json:"instances"`
}

type HostMdevParent struct {
	Parent   string         `json:"parent"`
	PCI      bool           `json:"pci"`
	VendorID *uint16        `json:"vendor_id,omitempty"`
	DeviceID *uint16        `json:"device_id,omitempty"`
	NUMANode *int           `json:"numa_node,omitempty"`
	Types    []HostMdevType `json:"types"`
}

type HostMdevType struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	DeviceAPI          string `json:"device_api,omitempty"`
	AvailableInstances int    `json:"available_instances"`
	FramebufferMiB     int64  `json:"framebuffer_mib,omitempty"`
	MIG                *struct {
		Slices    int `json:"slices"`
		MemoryGiB int `json:"memory_gib"`
	} `json:"mig,omitempty"`
}

type HostMdevInstance struct {
	UUID      string `json:"uuid"`
	Parent    string `json:"parent"`
	TypeID    string `json:"type_id"`
	Allocated bool   `json:"allocated"`
	Held      bool   `json:"held"`
}

// HostGPUHealth is one item of FluxVM's GET /v1/host/gpus/health.
type HostGPUHealth struct {
	BDF     string   `json:"bdf"`
	Status  string   `json:"status"`
	Reasons []string `json:"reasons,omitempty"`
	NVIDIA  *struct {
		MemoryTotalMiB int64  `json:"memory_total_mib,omitempty"`
		MemoryUsedMiB  int64  `json:"memory_used_mib,omitempty"`
		MIGMode        string `json:"mig_mode,omitempty"`
	} `json:"nvidia,omitempty"`
}

func (c *Client) GetHostMdev(ctx context.Context) (*HostMdev, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/host/mdev", nil)
	if err != nil {
		return nil, err
	}
	var out HostMdev
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM mdev inventory: %w", err)
	}
	return &out, nil
}

// CreateMdev creates mediated device uuid of typeID on parent. FluxVM
// treats an existing identical instance as success.
func (c *Client) CreateMdev(ctx context.Context, parent, typeID, uuid string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/host/mdev", map[string]string{"parent": parent, "type": typeID, "uuid": uuid})
	return err
}

// RemoveMdev removes a mediated device; FluxVM treats absent as success.
func (c *Client) RemoveMdev(ctx context.Context, uuid string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/host/mdev/"+url.PathEscape(uuid), nil)
	return err
}

func (c *Client) GetHostGPUHealth(ctx context.Context) ([]HostGPUHealth, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/host/gpus/health", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []HostGPUHealth `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM GPU health: %w", err)
	}
	return out.Items, nil
}

func healthStatus(s string) string {
	switch s {
	case "healthy":
		return model.DeviceHealthy
	case "degraded":
		return model.DeviceDegraded
	case "unhealthy":
		return model.DeviceUnhealthy
	}
	return ""
}

// ApplyGPUInventory adds GPU health and mediated device parents to a
// NodeTopology status. mdev or health may be nil (an older FluxVM). Only
// parents in allowParents are allocatable; nil allows every parent.
func ApplyGPUInventory(st *model.NodeTopologyStatus, mdev *HostMdev, health []HostGPUHealth, allowParents map[string]struct{}) {
	byBDF := map[string]HostGPUHealth{}
	for _, h := range health {
		byBDF[strings.ToLower(h.BDF)] = h
	}
	devIndex := map[string]int{}
	for i := range st.Devices {
		d := &st.Devices[i]
		devIndex[d.BDF] = i
		h, ok := byBDF[d.BDF]
		if !ok {
			continue
		}
		d.Health, d.HealthReasons = healthStatus(h.Status), h.Reasons
		if d.VRAMGiB == 0 && h.NVIDIA != nil && h.NVIDIA.MemoryTotalMiB > 0 {
			d.VRAMGiB = int((h.NVIDIA.MemoryTotalMiB + 512) / 1024)
		}
	}
	if mdev == nil {
		return
	}
	instances := map[string][]string{}
	for _, in := range mdev.Instances {
		instances[in.Parent] = append(instances[in.Parent], strings.ToLower(in.UUID))
	}
	for _, p := range mdev.Parents {
		tp := model.TopologyMediatedParent{Parent: p.Parent, Class: model.DeviceClassGPU, NUMANode: p.NUMANode, Instances: instances[p.Parent]}
		for _, t := range p.Types {
			// mtty and other non-PCI mdev APIs cannot be a guest PCI device.
			if t.DeviceAPI != "" && t.DeviceAPI != "vfio-pci" {
				continue
			}
			mt := model.TopologyMediatedType{ID: t.ID, Name: t.Name, Available: t.AvailableInstances, FramebufferMiB: t.FramebufferMiB}
			if t.MIG != nil {
				mt.MIGSlices, mt.MIGMemoryGiB = t.MIG.Slices, t.MIG.MemoryGiB
			}
			tp.Types = append(tp.Types, mt)
		}
		if len(tp.Types) == 0 {
			continue
		}
		if p.PCI {
			if p.VendorID != nil {
				tp.Vendor = fmt.Sprintf("%04x", *p.VendorID)
			}
			if p.DeviceID != nil {
				tp.Device = fmt.Sprintf("%04x", *p.DeviceID)
			}
			if i, ok := devIndex[p.Parent]; ok {
				d := st.Devices[i]
				tp.PCIeRoot = d.PCIeRoot
				if tp.NUMANode == nil {
					tp.NUMANode = d.NUMANode
				}
				tp.VRAMMiB = int64(d.VRAMGiB) * 1024
			}
			if h, ok := byBDF[p.Parent]; ok {
				tp.Health, tp.HealthReasons = healthStatus(h.Status), h.Reasons
				if h.NVIDIA != nil && h.NVIDIA.MemoryTotalMiB > 0 {
					tp.VRAMMiB = h.NVIDIA.MemoryTotalMiB
				}
			}
		}
		_, allowed := allowParents[p.Parent]
		tp.Allocatable = allowParents == nil || allowed
		st.MediatedDevices = append(st.MediatedDevices, tp)
	}
	sort.Slice(st.MediatedDevices, func(i, j int) bool { return st.MediatedDevices[i].Parent < st.MediatedDevices[j].Parent })
}
