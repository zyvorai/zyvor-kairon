// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "fmt"

const (
	KindMachineInstanceType        = "MachineInstanceType"
	KindMachineClusterInstanceType = "MachineClusterInstanceType"
	KindMachinePreference          = "MachinePreference"
	KindMachineClusterPreference   = "MachineClusterPreference"

	// AnnotationPreferenceApplied records the preference kairon-controller
	// applied to a Machine ("<kind>/<name>"), so it is applied once.
	AnnotationPreferenceApplied = "kairon.zyvor.dev/preference-applied"
)

// MachineInstanceType is a reusable, named CPU/memory shape a Machine
// references via spec.instanceTypeName instead of inlining spec.resources
// itself -- Kairon's equivalent of an EC2 instance type or KubeVirt's
// VirtualMachineInstancetype. OS-level defaults live in a separate
// MachinePreference, like KubeVirt's VirtualMachinePreference. See
// docs/guides/machine-instance-types.md.
type MachineInstanceType struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta              `json:"metadata"`
	Spec     MachineInstanceTypeSpec `json:"spec"`
}

func (t MachineInstanceType) Namespace() string {
	return t.Metadata.Namespace
}

type MachineInstanceTypeList struct {
	TypeMeta `json:",inline"`
	Items    []MachineInstanceType `json:"items"`
}

// MachineInstanceTypeSpec reuses ResourceSpec directly rather than
// duplicating a parallel Cpu/Memory pair -- the exact shape
// kairon-controller copies into a referencing Machine's own
// spec.resources.
type MachineInstanceTypeSpec struct {
	Resources ResourceSpec `json:"resources"`
}

// MachineClusterInstanceType is the cluster-scoped MachineInstanceType: one
// shared catalog every namespace can reference with
// spec.instanceTypeKind: MachineClusterInstanceType.
type MachineClusterInstanceType struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta              `json:"metadata"`
	Spec     MachineInstanceTypeSpec `json:"spec"`
}

type MachineClusterInstanceTypeList struct {
	TypeMeta `json:",inline"`
	Items    []MachineClusterInstanceType `json:"items"`
}

// MachinePreference holds guest-OS defaults a Machine picks up through
// spec.preferenceName. Each field only fills a Machine field that is
// still unset; a preference never overrides an explicit value.
type MachinePreference struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta            `json:"metadata"`
	Spec     MachinePreferenceSpec `json:"spec"`
}

func (p MachinePreference) Namespace() string {
	return p.Metadata.Namespace
}

type MachinePreferenceList struct {
	TypeMeta `json:",inline"`
	Items    []MachinePreference `json:"items"`
}

// MachineClusterPreference is the cluster-scoped MachinePreference.
type MachineClusterPreference struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta            `json:"metadata"`
	Spec     MachinePreferenceSpec `json:"spec"`
}

type MachineClusterPreferenceList struct {
	TypeMeta `json:",inline"`
	Items    []MachineClusterPreference `json:"items"`
}

// MachinePreferenceSpec covers the Machine fields that describe the guest
// OS rather than its size. Booleans can only turn a feature on: false is
// indistinguishable from unset on the Machine.
type MachinePreferenceSpec struct {
	// Backend fills spec.runtime.backend (qemu, cloud-hypervisor, ...).
	Backend string `json:"backend,omitempty"`
	// Architecture fills spec.placement.architecture.
	Architecture string `json:"architecture,omitempty"`
	// SecureBoot and TPM fill spec.security.
	SecureBoot bool `json:"secureBoot,omitempty"`
	TPM        bool `json:"tpm,omitempty"`
	// GuestAgent fills spec.guestAgent.enabled.
	GuestAgent bool `json:"guestAgent,omitempty"`
	// GuestOS is a free-form hint (e.g. "windows", "linux") shown by
	// kaironctl and the dashboard. It changes nothing on the Machine.
	GuestOS string `json:"guestOS,omitempty"`
}

// ValidateInstanceTypeRef checks spec.instanceTypeKind and
// spec.preferenceKind name a kind that exists.
func ValidateInstanceTypeRef(s MachineSpec) error {
	switch s.InstanceTypeKind {
	case "", KindMachineInstanceType, KindMachineClusterInstanceType:
	default:
		return fmt.Errorf("spec.instanceTypeKind %q: must be %s or %s", s.InstanceTypeKind, KindMachineInstanceType, KindMachineClusterInstanceType)
	}
	if s.InstanceTypeKind != "" && s.InstanceTypeName == "" {
		return fmt.Errorf("spec.instanceTypeKind needs spec.instanceTypeName")
	}
	switch s.PreferenceKind {
	case "", KindMachinePreference, KindMachineClusterPreference:
	default:
		return fmt.Errorf("spec.preferenceKind %q: must be %s or %s", s.PreferenceKind, KindMachinePreference, KindMachineClusterPreference)
	}
	if s.PreferenceKind != "" && s.PreferenceName == "" {
		return fmt.Errorf("spec.preferenceKind needs spec.preferenceName")
	}
	return nil
}

// ApplyPreference fills the Machine fields p covers that are still unset
// and reports whether anything changed.
func ApplyPreference(s *MachineSpec, p MachinePreferenceSpec) bool {
	changed := false
	if p.Backend != "" && s.Runtime.Backend == "" {
		s.Runtime.Backend = p.Backend
		changed = true
	}
	if p.Architecture != "" && s.Placement.Architecture == "" {
		s.Placement.Architecture = p.Architecture
		changed = true
	}
	if p.SecureBoot && !s.Security.SecureBoot {
		s.Security.SecureBoot = true
		changed = true
	}
	if p.TPM && !s.Security.TPM {
		s.Security.TPM = true
		changed = true
	}
	if p.GuestAgent && !s.GuestAgent.Enabled {
		s.GuestAgent.Enabled = true
		changed = true
	}
	return changed
}
