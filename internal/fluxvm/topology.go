// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/model"
)

// RuntimeHostCapability is FluxVM's host-placement capability block.
type RuntimeHostCapability struct {
	Topology    bool `json:"topology"`
	NUMABinding bool `json:"numaBinding"`
	PCIBind     bool `json:"pciBind"`
	// Balloon: QEMU balloon control and GET /v1/host/memory.
	Balloon bool `json:"balloon"`
}

// HostTopology is FluxVM's GET /v1/host/topology.
type HostTopology struct {
	NUMANodes []HostNUMANode `json:"numaNodes"`
	Devices   []HostDevice   `json:"devices"`
}

type HostNUMANode struct {
	ID            int             `json:"id"`
	CPUs          []uint32        `json:"cpus"`
	MemoryMiB     int64           `json:"memoryMib"`
	FreeMemoryMiB int64           `json:"freeMemoryMib"`
	Hugepages     []HostHugepages `json:"hugepages"`
	Distances     []int           `json:"distances,omitempty"`
}

type HostHugepages struct {
	SizeKiB int64 `json:"sizeKib"`
	Total   int64 `json:"total"`
	Free    int64 `json:"free"`
}

type HostDevice struct {
	BDF           string     `json:"bdf"`
	Class         string     `json:"class"`
	VendorID      uint16     `json:"vendorId"`
	DeviceID      uint16     `json:"deviceId"`
	Driver        string     `json:"driver,omitempty"`
	NUMANode      *int       `json:"numaNode,omitempty"`
	PCIeRoot      string     `json:"pcieRoot"`
	IOMMUGroup    *int       `json:"iommuGroup,omitempty"`
	SRIOV         *HostSRIOV `json:"sriov,omitempty"`
	PhysFn        string     `json:"physfn,omitempty"`
	NetDevs       []string   `json:"netdevs,omitempty"`
	LinkSpeedMbps int        `json:"linkSpeedMbps,omitempty"`
	SizeBytes     int64      `json:"sizeBytes,omitempty"`
	VRAMGiB       int        `json:"vramGib,omitempty"`
	Allocated     bool       `json:"allocated"`
}

type HostSRIOV struct {
	TotalVFs int      `json:"totalVfs"`
	NumVFs   int      `json:"numVfs"`
	VFs      []string `json:"vfs"`
}

// NUMANode is one CreateVmRequest.numa entry: a guest NUMA node bound to
// a host node.
type NUMANode struct {
	HostNode  int      `json:"host_node"`
	VCPUs     []uint32 `json:"vcpus"`
	HostCPUs  []uint32 `json:"host_cpus,omitempty"`
	MemoryMiB int64    `json:"memory_mib"`
}

// VMNUMAStatus is FluxVM's GET /v1/vms/{id}/numa.
type VMNUMAStatus struct {
	Nodes []NUMANode `json:"nodes"`
	VCPUs []struct {
		VCPU     uint32   `json:"vcpu"`
		ThreadID int      `json:"thread_id"`
		HostNode int      `json:"host_node"`
		HostCPUs []uint32 `json:"host_cpus"`
	} `json:"vcpus"`
	Memdevs []struct {
		ID        string `json:"id"`
		SizeMiB   int64  `json:"size_mib"`
		HostNodes []int  `json:"host_nodes"`
		Policy    string `json:"policy"`
	} `json:"memdevs"`
}

func (c *Client) GetHostTopology(ctx context.Context) (*HostTopology, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/host/topology", nil)
	if err != nil {
		return nil, err
	}
	var out HostTopology
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM host topology: %w", err)
	}
	return &out, nil
}

// BindHostPCI moves bdf's IOMMU group to vfio-pci (FluxVM admin route).
func (c *Client) BindHostPCI(ctx context.Context, bdf string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/host/pci/bind", map[string]any{"bdf": bdf})
	return err
}

func (c *Client) GetVMNUMA(ctx context.Context, id string) (*VMNUMAStatus, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/numa", nil)
	if err != nil {
		return nil, err
	}
	var out VMNUMAStatus
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode FluxVM VM NUMA status: %w", err)
	}
	return &out, nil
}

// numaFromAllocation turns spec.topologyAllocation into CreateVmRequest.numa:
// guest node i gets the next len(CPUs) vCPUs, pinned to that host node's
// allocated CPUs, and its share of memory.
func numaFromAllocation(alloc *model.TopologyAllocation, vcpus uint32, memMiB uint64) ([]NUMANode, error) {
	if alloc == nil || len(alloc.NUMANodes) == 0 {
		return nil, nil
	}
	var out []NUMANode
	var next uint32
	var mem int64
	for _, n := range alloc.NUMANodes {
		cpus, err := model.ParseCPUList(n.CPUs)
		if err != nil {
			return nil, fmt.Errorf("spec.topologyAllocation numa%d: %w", n.ID, err)
		}
		node := NUMANode{HostNode: n.ID, HostCPUs: cpus, MemoryMiB: n.MemoryMiB}
		for range cpus {
			node.VCPUs = append(node.VCPUs, next)
			next++
		}
		mem += n.MemoryMiB
		out = append(out, node)
	}
	if next != vcpus {
		return nil, fmt.Errorf("spec.topologyAllocation pins %d vCPUs but the Machine boots with %d", next, vcpus)
	}
	if uint64(mem) != memMiB {
		return nil, fmt.Errorf("spec.topologyAllocation places %dMiB but the Machine boots with %dMiB", mem, memMiB)
	}
	return out, nil
}

// NodeTopologyStatus maps FluxVM's host topology onto a NodeTopology
// status. Only devices in allowlist are allocatable.
func NodeTopologyStatus(t *HostTopology, numaBinding bool, allowlist map[string]struct{}) model.NodeTopologyStatus {
	var st model.NodeTopologyStatus
	st.NUMABinding = numaBinding
	for _, n := range t.NUMANodes {
		node := model.TopologyNUMANode{
			ID: n.ID, CPUs: model.FormatCPUList(n.CPUs), MemoryMiB: n.MemoryMiB,
			FreeMemoryMiB: n.FreeMemoryMiB, Distances: n.Distances,
		}
		for _, h := range n.Hugepages {
			size := ""
			switch h.SizeKiB {
			case 2048:
				size = "2Mi"
			case 1048576:
				size = "1Gi"
			default:
				continue
			}
			node.Hugepages = append(node.Hugepages, model.TopologyHugepages{Size: size, Total: h.Total, Free: h.Free})
		}
		st.NUMANodes = append(st.NUMANodes, node)
	}
	for _, d := range t.Devices {
		_, allowed := allowlist[d.BDF]
		dev := model.TopologyDevice{
			BDF: d.BDF, Class: d.Class,
			Vendor: fmt.Sprintf("%04x", d.VendorID), Device: fmt.Sprintf("%04x", d.DeviceID),
			Driver: d.Driver, NUMANode: d.NUMANode, PCIeRoot: d.PCIeRoot, IOMMUGroup: d.IOMMUGroup,
			PhysFn: d.PhysFn, NetDevs: d.NetDevs, LinkSpeedMbps: d.LinkSpeedMbps,
			SizeBytes: d.SizeBytes, VRAMGiB: d.VRAMGiB,
			Allocatable: allowed, Allocated: d.Allocated,
		}
		if d.SRIOV != nil {
			dev.TotalVFs = d.SRIOV.TotalVFs
		}
		st.Devices = append(st.Devices, dev)
	}
	return st
}
