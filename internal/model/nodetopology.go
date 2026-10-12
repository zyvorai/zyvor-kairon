// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"reflect"
	"regexp"
	"time"
)

const KindNodeTopology = "NodeTopology"

// NodeTopology is a node's hardware graph as FluxVM reports it: NUMA nodes
// with their CPUs, memory and hugepages, and the PCI devices a Machine can
// be given, each with its NUMA node and PCIe root. Cluster-scoped and named
// after the node; kairon-node owns it and writes only status.
type NodeTopology struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Status   NodeTopologyStatus `json:"status,omitempty"`
}

type NodeTopologyList struct {
	TypeMeta `json:",inline"`
	Items    []NodeTopology `json:"items"`
}

type NodeTopologyStatus struct {
	NUMANodes []TopologyNUMANode `json:"numaNodes,omitempty"`
	Devices   []TopologyDevice   `json:"devices,omitempty"`
	// NUMABinding is true when the node's FluxVM binds guest memory and
	// pins vCPU threads; the scheduler refuses hard topology policies on
	// a node without it.
	NUMABinding bool `json:"numaBinding,omitempty"`
	// Hibernation is set when the node's FluxVM can hibernate; the
	// controller compares it with a Machine's saved state before resuming
	// it here.
	Hibernation *HibernationHostFacts `json:"hibernation,omitempty"`
	ObservedAt  *time.Time            `json:"observedAt,omitempty"`
}

type TopologyNUMANode struct {
	ID int `json:"id"`
	// CPUs is a kernel CPU list, e.g. "0-11,24-35".
	CPUs          string              `json:"cpus"`
	MemoryMiB     int64               `json:"memoryMiB"`
	FreeMemoryMiB int64               `json:"freeMemoryMiB,omitempty"`
	Hugepages     []TopologyHugepages `json:"hugepages,omitempty"`
	Distances     []int               `json:"distances,omitempty"`
}

type TopologyHugepages struct {
	// Size is "2Mi" or "1Gi".
	Size  string `json:"size"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
}

const (
	DeviceClassGPU         = "gpu"
	DeviceClassNIC         = "nic"
	DeviceClassNVMe        = "nvme"
	DeviceClassAccelerator = "accelerator"
)

type TopologyDevice struct {
	BDF   string `json:"bdf"`
	Class string `json:"class"`
	// Vendor and Device are PCI IDs as 4 hex digits, e.g. "10de".
	Vendor     string `json:"vendor"`
	Device     string `json:"device"`
	Driver     string `json:"driver,omitempty"`
	NUMANode   *int   `json:"numaNode,omitempty"`
	PCIeRoot   string `json:"pcieRoot"`
	IOMMUGroup *int   `json:"iommuGroup,omitempty"`
	// PhysFn is set on an SR-IOV virtual function: its PF's BDF.
	PhysFn        string   `json:"physfn,omitempty"`
	TotalVFs      int      `json:"totalVFs,omitempty"`
	NetDevs       []string `json:"netdevs,omitempty"`
	LinkSpeedMbps int      `json:"linkSpeedMbps,omitempty"`
	SizeBytes     int64    `json:"sizeBytes,omitempty"`
	VRAMGiB       int      `json:"vramGiB,omitempty"`
	// Allocatable is true only for devices in kairon-node's
	// --vfio-allowlist, so the scheduler never hands out a device the
	// operator did not allow.
	Allocatable bool `json:"allocatable"`
	// Allocated means FluxVM already has the device in some VM.
	Allocated bool `json:"allocated,omitempty"`
}

const (
	TopologyPolicyNone           = "none"
	TopologyPolicyBestEffort     = "best-effort"
	TopologyPolicyRestricted     = "restricted"
	TopologyPolicySingleNUMANode = "single-numa-node"

	AlignSameNUMA     = "sameNuma"
	AlignSamePCIeRoot = "samePcieRoot"
)

// TopologySpec asks the scheduler to place a Machine's CPUs, memory and
// devices by host NUMA locality.
type TopologySpec struct {
	// Policy is none (the default), best-effort (prefer locality, never
	// refuse a node for it), restricted (the fewest NUMA nodes that fit,
	// devices local to one of them) or single-numa-node (everything on
	// one node, else the node is ineligible).
	Policy string `json:"policy,omitempty"`
	// HugepageSize is the page size to account guest memory against:
	// "2Mi" or "1Gi". Requires spec.resources.hugepages.
	HugepageSize string `json:"hugepageSize,omitempty"`
}

// MachineDevice requests PCI devices from the node's NodeTopology, passed
// through to the guest with VFIO (qemu backend).
type MachineDevice struct {
	Name  string `json:"name"`
	Class string `json:"class"`
	// Vendor is a PCI vendor ID ("10de") or one of nvidia, amd, intel,
	// mellanox.
	Vendor string `json:"vendor,omitempty"`
	// Model is a PCI device ID, e.g. "2330".
	Model string `json:"model,omitempty"`
	Count int    `json:"count,omitempty"`
	// MinMemory is the least GPU memory per device, e.g. "40Gi".
	MinMemory string `json:"minMemory,omitempty"`
	// VirtualFunction takes SR-IOV virtual functions instead of whole
	// devices.
	VirtualFunction bool `json:"virtualFunction,omitempty"`
	// Alignment: sameNuma keeps every device of this request on one NUMA
	// node with the Machine's CPUs; samePcieRoot also puts them under
	// one PCIe root (GPU and NIC peer to peer).
	Alignment []string `json:"alignment,omitempty"`
}

// TopologyAllocation is the scheduler's decision, patched together with
// spec.nodeName and spec.resources.allocatedCpuSet. Not user-set.
type TopologyAllocation struct {
	NUMANodes []AllocatedNUMANode `json:"numaNodes,omitempty"`
	Devices   []AllocatedDevice   `json:"devices,omitempty"`
}

type AllocatedNUMANode struct {
	ID        int    `json:"id"`
	CPUs      string `json:"cpus"`
	MemoryMiB int64  `json:"memoryMiB"`
	// Hugepages is the number of pages of spec.topology.hugepageSize.
	Hugepages int64 `json:"hugepages,omitempty"`
}

type AllocatedDevice struct {
	Name     string `json:"name"`
	BDF      string `json:"bdf"`
	NUMANode *int   `json:"numaNode,omitempty"`
	PCIeRoot string `json:"pcieRoot,omitempty"`
}

// AppliedTopology is what FluxVM reported after the VM started.
type AppliedTopology struct {
	NUMANodes  []AppliedNUMANode `json:"numaNodes,omitempty"`
	Devices    []string          `json:"devices,omitempty"`
	ObservedAt *time.Time        `json:"observedAt,omitempty"`
	// Message explains a mismatch with spec.topologyAllocation.
	Message string `json:"message,omitempty"`
}

type AppliedNUMANode struct {
	HostNode  int    `json:"hostNode"`
	VCPUs     string `json:"vcpus"`
	HostCPUs  string `json:"hostCpus"`
	MemoryMiB int64  `json:"memoryMiB"`
	Policy    string `json:"policy"`
}

// TopologyPolicy is spec.topology.policy with the default filled in.
func (s MachineSpec) TopologyPolicy() string {
	if s.Topology == nil || s.Topology.Policy == "" {
		return TopologyPolicyNone
	}
	return s.Topology.Policy
}

// NeedsTopology reports whether the scheduler places this Machine with the
// topology allocator rather than the plain CPU allocator.
func (s MachineSpec) NeedsTopology() bool {
	return s.TopologyPolicy() != TopologyPolicyNone || len(s.Devices) > 0
}

// HardTopology reports whether locality can make a node ineligible.
func (s MachineSpec) HardTopology() bool {
	p := s.TopologyPolicy()
	return p == TopologyPolicyRestricted || p == TopologyPolicySingleNUMANode
}

var deviceNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
var pciIDRE = regexp.MustCompile(`^[0-9a-f]{4}$`)

var vendorNames = map[string]string{
	"nvidia":   "10de",
	"amd":      "1002",
	"intel":    "8086",
	"mellanox": "15b3",
}

// PCIVendorID resolves MachineDevice.Vendor to a PCI vendor ID.
func PCIVendorID(v string) string {
	if id, ok := vendorNames[v]; ok {
		return id
	}
	return v
}

// ValidateTopology checks spec.topology and spec.devices. The CRD's CEL
// rules cover the same ground for clients that skip the webhook.
func ValidateTopology(s MachineSpec) error {
	if t := s.Topology; t != nil {
		switch t.Policy {
		case "", TopologyPolicyNone, TopologyPolicyBestEffort, TopologyPolicyRestricted, TopologyPolicySingleNUMANode:
		default:
			return fmt.Errorf("spec.topology.policy %q: want none, best-effort, restricted or single-numa-node", t.Policy)
		}
		switch t.HugepageSize {
		case "":
		case "2Mi", "1Gi":
			if !s.Resources.Hugepages {
				return fmt.Errorf("spec.topology.hugepageSize needs spec.resources.hugepages: true")
			}
		default:
			return fmt.Errorf("spec.topology.hugepageSize %q: want 2Mi or 1Gi", t.HugepageSize)
		}
	}
	if s.TopologyPolicy() != TopologyPolicyNone {
		if !s.Resources.CPUPinning {
			return fmt.Errorf("spec.topology.policy %s needs spec.resources.cpuPinning: true", s.TopologyPolicy())
		}
		if s.Resources.NUMANode != nil || s.Resources.CPUSet != "" {
			return fmt.Errorf("spec.topology.policy replaces spec.resources.numaNode and cpuSet; unset them")
		}
	}
	if len(s.Devices) == 0 {
		return nil
	}
	switch s.Runtime.Backend {
	case "", "auto", "qemu":
	default:
		return fmt.Errorf("spec.devices needs the qemu backend, not %q", s.Runtime.Backend)
	}
	seen := map[string]bool{}
	for i, d := range s.Devices {
		at := fmt.Sprintf("spec.devices[%d]", i)
		if !deviceNameRE.MatchString(d.Name) {
			return fmt.Errorf("%s.name %q: want a DNS label of at most 32 characters", at, d.Name)
		}
		if seen[d.Name] {
			return fmt.Errorf("%s.name %q is used twice", at, d.Name)
		}
		seen[d.Name] = true
		switch d.Class {
		case DeviceClassGPU, DeviceClassNIC, DeviceClassNVMe, DeviceClassAccelerator:
		default:
			return fmt.Errorf("%s.class %q: want gpu, nic, nvme or accelerator", at, d.Class)
		}
		if d.Count < 0 || d.Count > 16 {
			return fmt.Errorf("%s.count %d: want 1-16", at, d.Count)
		}
		if d.Vendor != "" && !pciIDRE.MatchString(PCIVendorID(d.Vendor)) {
			return fmt.Errorf("%s.vendor %q: want 4 hex digits or nvidia, amd, intel, mellanox", at, d.Vendor)
		}
		if d.Model != "" && !pciIDRE.MatchString(d.Model) {
			return fmt.Errorf("%s.model %q: want a 4 hex digit PCI device ID", at, d.Model)
		}
		if d.MinMemory != "" {
			if d.Class != DeviceClassGPU {
				return fmt.Errorf("%s.minMemory only applies to class gpu", at)
			}
			if _, err := ParseMemoryMiB(d.MinMemory); err != nil {
				return fmt.Errorf("%s.minMemory: %v", at, err)
			}
		}
		for _, a := range d.Alignment {
			if a != AlignSameNUMA && a != AlignSamePCIeRoot {
				return fmt.Errorf("%s.alignment %q: want sameNuma or samePcieRoot", at, a)
			}
		}
	}
	return nil
}

// ValidateTopologyUpdate refuses changing spec.topology or spec.devices
// once the Machine is scheduled.
func ValidateTopologyUpdate(old, cur MachineSpec) error {
	if old.NodeName == "" {
		return nil
	}
	if !reflect.DeepEqual(old.Topology, cur.Topology) || !reflect.DeepEqual(old.Devices, cur.Devices) {
		return fmt.Errorf("spec.topology and spec.devices cannot change after the Machine is scheduled")
	}
	return nil
}

// DeviceCount is d.Count with the default of 1.
func (d MachineDevice) DeviceCount() int {
	if d.Count <= 0 {
		return 1
	}
	return d.Count
}

func (d MachineDevice) Aligned(kind string) bool {
	for _, a := range d.Alignment {
		if a == kind {
			return true
		}
	}
	return false
}
