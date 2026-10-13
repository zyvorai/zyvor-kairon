// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const KindMachineAcceleratorProfile = "MachineAcceleratorProfile"

// AnnotationAccelerators records which profiles kairon-controller expanded
// into spec.devices, as name=profile@generation pairs. Set only by the
// controller; a Machine created with it is refused.
const AnnotationAccelerators = "kairon.zyvor.dev/accelerators"

// MachineAcceleratorProfile is a named, cluster-scoped accelerator shape
// that Machines ask for with spec.accelerators: how many devices of which
// kind, how they must sit relative to each other, and an optional RDMA NIC
// next to them.
type MachineAcceleratorProfile struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                      `json:"metadata"`
	Spec     MachineAcceleratorProfileSpec   `json:"spec"`
	Status   MachineAcceleratorProfileStatus `json:"status,omitempty"`
}

type MachineAcceleratorProfileList struct {
	TypeMeta `json:",inline"`
	Items    []MachineAcceleratorProfile `json:"items"`
}

type MachineAcceleratorProfileSpec struct {
	// Class is gpu (the default) or accelerator.
	Class string `json:"class,omitempty"`
	// Vendor is a PCI vendor ID or nvidia, amd, intel.
	Vendor string `json:"vendor,omitempty"`
	// Model is a PCI device ID.
	Model string `json:"model,omitempty"`
	// Profile asks for vGPU (mediated device) instances of this type
	// instead of whole devices.
	Profile string `json:"profile,omitempty"`
	// Count is the number of devices, default 1.
	Count     int                   `json:"count,omitempty"`
	Topology  *AcceleratorTopology  `json:"topology,omitempty"`
	Memory    *AcceleratorMemory    `json:"memory,omitempty"`
	NIC       *AcceleratorNIC       `json:"nic,omitempty"`
	Migration *AcceleratorMigration `json:"migration,omitempty"`
}

type AcceleratorTopology struct {
	// P2PRequired puts every device under one PCIe root (one parent for
	// vGPU instances of a parent that is not a PCI device).
	P2PRequired bool `json:"p2pRequired,omitempty"`
	// SameNUMA keeps the devices on one NUMA node.
	SameNUMA bool `json:"sameNuma,omitempty"`
}

type AcceleratorMemory struct {
	// MinimumPerDevice is the least GPU memory (or vGPU framebuffer) per
	// device, e.g. "10Gi".
	MinimumPerDevice string `json:"minimumPerDevice,omitempty"`
}

// AcceleratorNIC adds NICs under the same PCIe root as the accelerators,
// for GPUDirect RDMA.
type AcceleratorNIC struct {
	Vendor          string `json:"vendor,omitempty"`
	Model           string `json:"model,omitempty"`
	VirtualFunction bool   `json:"virtualFunction,omitempty"`
	Count           int    `json:"count,omitempty"`
}

type AcceleratorMigration struct {
	// Required asks for live migration. Passthrough and vGPU devices
	// cannot live-migrate yet, so a profile with it is not Ready.
	Required bool `json:"required,omitempty"`
}

type MachineAcceleratorProfileStatus struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty"`
	// Nodes is how many more Machines with this profile each node's free
	// devices could take, counting devices only (not CPUs or memory).
	Nodes []AcceleratorNodeCapacity `json:"nodes,omitempty"`
	// Capacity is the sum of Nodes[].fits.
	Capacity int `json:"capacity"`
	// Machines is how many Machines were expanded from this profile.
	Machines int `json:"machines"`
}

type AcceleratorNodeCapacity struct {
	Node string `json:"node"`
	Fits int    `json:"fits"`
	// Reason says why nothing more fits.
	Reason string `json:"reason,omitempty"`
}

// MachineAccelerator is one spec.accelerators entry.
type MachineAccelerator struct {
	// Name names the spec.devices entries the profile expands to: Name for
	// the accelerators, Name-nic for the NICs.
	Name string `json:"name"`
	// Profile is the MachineAcceleratorProfile's name.
	Profile string `json:"profile"`
}

// Condition types and reasons on MachineAcceleratorProfile, and the
// Machine's Scheduled=False reason while a profile cannot be expanded.
const (
	AcceleratorProfileReady             = "Ready"
	AcceleratorProfileAvailable         = "Available"
	AcceleratorProfileReasonValid       = "Valid"
	AcceleratorProfileReasonInvalid     = "Invalid"
	AcceleratorProfileReasonLiveMigrate = "LiveMigrationUnsupported"
	AcceleratorProfileReasonFits        = "Fits"
	AcceleratorProfileReasonNoCapacity  = "NoCapacity"
	ReasonAcceleratorProfileUnavailable = "AcceleratorProfileUnavailable"
)

const (
	maxAcceleratorsPerMachine = 4
	maxAcceleratorDevices     = 16
	acceleratorNICSuffix      = "-nic"
	vendorWant                = "4 hex digits or nvidia, amd, intel, mellanox"
)

// Accelerator names leave room for the -nic suffix within a device name.
var acceleratorNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,26}[a-z0-9])?$`)
var acceleratorProfileNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

func (s MachineAcceleratorProfileSpec) class() string {
	if s.Class == "" {
		return DeviceClassGPU
	}
	return s.Class
}

// Validate checks the spec. It does not refuse migration.required; see
// Usable.
func (s MachineAcceleratorProfileSpec) Validate() error {
	switch s.class() {
	case DeviceClassGPU, DeviceClassAccelerator:
	default:
		return fmt.Errorf("spec.class %q: want gpu or accelerator", s.Class)
	}
	if s.Count < 0 || s.Count > maxAcceleratorDevices {
		return fmt.Errorf("spec.count %d: want 1-%d", s.Count, maxAcceleratorDevices)
	}
	if s.Vendor != "" && !pciIDRE.MatchString(PCIVendorID(s.Vendor)) {
		return fmt.Errorf("spec.vendor %q: want %s", s.Vendor, vendorWant)
	}
	if s.Model != "" && !pciIDRE.MatchString(s.Model) {
		return fmt.Errorf("spec.model %q: want a 4 hex digit PCI device ID", s.Model)
	}
	if s.Profile != "" && !profileRE.MatchString(s.Profile) {
		return fmt.Errorf("spec.profile %q: want an mdev type ID or profile name of at most 64 characters", s.Profile)
	}
	if s.Memory != nil && s.Memory.MinimumPerDevice != "" {
		if s.class() != DeviceClassGPU {
			return fmt.Errorf("spec.memory.minimumPerDevice only applies to class gpu")
		}
		if _, err := ParseMemoryMiB(s.Memory.MinimumPerDevice); err != nil {
			return fmt.Errorf("spec.memory.minimumPerDevice: %v", err)
		}
	}
	if n := s.NIC; n != nil {
		if n.Count < 0 || n.Count > maxAcceleratorDevices {
			return fmt.Errorf("spec.nic.count %d: want 1-%d", n.Count, maxAcceleratorDevices)
		}
		if n.Vendor != "" && !pciIDRE.MatchString(PCIVendorID(n.Vendor)) {
			return fmt.Errorf("spec.nic.vendor %q: want %s", n.Vendor, vendorWant)
		}
		if n.Model != "" && !pciIDRE.MatchString(n.Model) {
			return fmt.Errorf("spec.nic.model %q: want a 4 hex digit PCI device ID", n.Model)
		}
	}
	return nil
}

// Usable reports whether Machines can be given this profile: a valid spec
// that does not ask for live migration. reason is a condition reason.
func (s MachineAcceleratorProfileSpec) Usable() (reason, message string) {
	if err := s.Validate(); err != nil {
		return AcceleratorProfileReasonInvalid, err.Error()
	}
	if s.Migration != nil && s.Migration.Required {
		return AcceleratorProfileReasonLiveMigrate, "migration.required: passthrough and vGPU devices cannot live-migrate yet"
	}
	return "", ""
}

// Devices expands the profile into spec.devices entries named name and
// name-nic. The NICs and, with p2pRequired, the accelerators get
// samePcieRoot; sameNuma goes on both.
func (s MachineAcceleratorProfileSpec) Devices(name string) []MachineDevice {
	var align []string
	if s.Topology != nil && s.Topology.SameNUMA {
		align = append(align, AlignSameNUMA)
	}
	rooted := s.NIC != nil || (s.Topology != nil && s.Topology.P2PRequired)
	if rooted {
		align = append(align, AlignSamePCIeRoot)
	}
	acc := MachineDevice{
		Name: name, Class: s.class(), Vendor: s.Vendor, Model: s.Model,
		Count: s.Count, Profile: s.Profile, Alignment: align,
	}
	if s.Memory != nil {
		acc.MinMemory = s.Memory.MinimumPerDevice
	}
	out := []MachineDevice{acc}
	if n := s.NIC; n != nil {
		out = append(out, MachineDevice{
			Name: name + acceleratorNICSuffix, Class: DeviceClassNIC, Vendor: n.Vendor, Model: n.Model,
			Count: n.Count, VirtualFunction: n.VirtualFunction, Alignment: append([]string(nil), align...),
		})
	}
	return out
}

// ValidateAccelerators checks spec.accelerators on a new Machine.
func ValidateAccelerators(s MachineSpec, annotations map[string]string) error {
	if len(s.Accelerators) == 0 {
		return nil
	}
	if _, ok := annotations[AnnotationAccelerators]; ok {
		return fmt.Errorf("metadata.annotations[%s] is set by kairon-controller", AnnotationAccelerators)
	}
	if s.NodeName != "" {
		return fmt.Errorf("spec.accelerators are placed by the scheduler; unset spec.nodeName")
	}
	if len(s.Accelerators) > maxAcceleratorsPerMachine {
		return fmt.Errorf("spec.accelerators: at most %d entries", maxAcceleratorsPerMachine)
	}
	switch s.Runtime.Backend {
	case "", "auto", "qemu":
	default:
		return fmt.Errorf("spec.accelerators needs the qemu backend, not %q", s.Runtime.Backend)
	}
	taken := map[string]bool{}
	for _, d := range s.Devices {
		taken[d.Name] = true
	}
	for i, a := range s.Accelerators {
		at := fmt.Sprintf("spec.accelerators[%d]", i)
		if !acceleratorNameRE.MatchString(a.Name) {
			return fmt.Errorf("%s.name %q: want a DNS label of at most 28 characters", at, a.Name)
		}
		if !acceleratorProfileNameRE.MatchString(a.Profile) {
			return fmt.Errorf("%s.profile %q: want a MachineAcceleratorProfile name", at, a.Profile)
		}
		for _, n := range []string{a.Name, a.Name + acceleratorNICSuffix} {
			if taken[n] {
				return fmt.Errorf("%s.name %q: spec.devices or another accelerator already uses %q", at, a.Name, n)
			}
			taken[n] = true
		}
	}
	return nil
}

// AcceleratorsResolved reports whether kairon-controller has expanded
// spec.accelerators into spec.devices.
func (m Machine) AcceleratorsResolved() bool {
	if len(m.Spec.Accelerators) == 0 {
		return true
	}
	_, ok := m.Metadata.Annotations[AnnotationAccelerators]
	return ok
}

// FormatAcceleratorRecord is the AnnotationAccelerators value: entries
// "name=profile@generation", sorted.
func FormatAcceleratorRecord(entries map[string]string) string {
	out := make([]string, 0, len(entries))
	for k, v := range entries {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// AcceleratorProfilesOf parses AnnotationAccelerators into the profile
// name of each entry.
func AcceleratorProfilesOf(m Machine) []string {
	v := m.Metadata.Annotations[AnnotationAccelerators]
	if v == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(v, ",") {
		_, rest, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		p, _, _ := strings.Cut(rest, "@")
		out = append(out, p)
	}
	return out
}
