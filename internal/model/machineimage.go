// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"fmt"
	"strings"
)

const KindMachineImage = "MachineImage"

const (
	MachineImageKindDisk = "disk"
	MachineImageKindISO  = "iso"
)

// MachineImage is a cluster-scoped, named and versioned image: a boot disk
// (kind disk) or install media (kind iso). A Machine names it with
// spec.image.imageRef or spec.cdroms[].imageRef; kairon-controller copies
// its source and digest into the Machine once, so republishing an image
// never changes what an existing Machine boots. Kairon's equivalent of a
// CDI DataSource, without a PVC per image. See docs/guides/machine-images.md.
type MachineImage struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta       `json:"metadata"`
	Spec     MachineImageSpec `json:"spec"`
}

type MachineImageList struct {
	TypeMeta `json:",inline"`
	Items    []MachineImage `json:"items"`
}

type MachineImageSpec struct {
	DisplayName string `json:"displayName,omitempty"`
	// Family groups versions of one image (e.g. windows-server-2022); Version
	// orders them. Neither is interpreted by Kairon.
	Family  string `json:"family,omitempty"`
	Version string `json:"version,omitempty"`
	// Kind is disk (default) or iso.
	Kind string `json:"kind,omitempty"`
	// OS is a hint for clients (linux, windows); Kairon does not read it.
	OS     string      `json:"os,omitempty"`
	Source ImageSource `json:"source"`
	Digest string      `json:"digest"`
	// Defaults fill an empty spec.resources / spec.image.diskSize on a
	// Machine that references this image (never overriding what it set).
	Defaults   MachineImageDefaults `json:"defaults,omitempty"`
	Deprecated bool                 `json:"deprecated,omitempty"`
}

type MachineImageDefaults struct {
	CPU      string `json:"cpu,omitempty"`
	Memory   string `json:"memory,omitempty"`
	DiskSize string `json:"diskSize,omitempty"`
}

// EffectiveKind returns Kind with the disk default applied.
func (s MachineImageSpec) EffectiveKind() string {
	if s.Kind == "" {
		return MachineImageKindDisk
	}
	return s.Kind
}

// Validate checks the image the same way a Machine's spec.image.source is
// checked, plus the kind-specific rules: install media is attached as-is,
// so it must be raw bytes with no conversion or repair.
func (s MachineImageSpec) Validate() error {
	switch s.EffectiveKind() {
	case MachineImageKindDisk:
	case MachineImageKindISO:
		if s.Source.OCI != "" {
			return fmt.Errorf("spec.source.oci: iso images must be served over http(s)")
		}
		if s.Source.Format != "" && s.Source.Format != "raw" {
			return fmt.Errorf("spec.source.format %q: iso images are attached as raw bytes", s.Source.Format)
		}
		if s.Source.Repair {
			return fmt.Errorf("spec.source.repair: install media can't be repaired")
		}
	default:
		return fmt.Errorf("spec.kind %q must be disk or iso", s.Kind)
	}
	src := s.Source
	if err := ValidateImageSource(ImageSpec{Source: &src, Digest: s.Digest}); err != nil {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "spec.image.", "spec."))
	}
	if s.Defaults.DiskSize != "" {
		if _, err := ParseDiskSizeGiB(s.Defaults.DiskSize); err != nil {
			return fmt.Errorf("spec.defaults.diskSize: %w", err)
		}
	}
	return nil
}
