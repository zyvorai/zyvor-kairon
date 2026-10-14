// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"time"
)

const (
	KindMachineExport = "MachineExport"

	DefaultExportTTL = 2 * time.Hour
	MaxExportTTL     = 7 * 24 * time.Hour
)

// MachineExport asks the node holding a Machine's boot disk to serve it for
// download. The Machine must be Stopped or Halted while the export is Ready.
type MachineExport struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta          `json:"metadata"`
	Spec     MachineExportSpec   `json:"spec"`
	Status   MachineExportStatus `json:"status,omitempty"`
}

type MachineExportList struct {
	Items []MachineExport `json:"items"`
}

type MachineExportSpec struct {
	MachineName string `json:"machineName"`
	// Format is qcow2 or raw; empty serves the disk in its own format.
	Format string `json:"format,omitempty"`
	// TTL is a Go duration counted from creation; default 2h, max 168h.
	TTL string `json:"ttl,omitempty"`
}

type MachineExportStatus struct {
	// Phase is Pending, Preparing, Ready, Expired or Failed.
	Phase        string     `json:"phase,omitempty"`
	NodeName     string     `json:"nodeName,omitempty"`
	Format       string     `json:"format,omitempty"`
	SizeBytes    int64      `json:"sizeBytes,omitempty"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	DownloadPath string     `json:"downloadPath,omitempty"`
	Message      string     `json:"message,omitempty"`
}

func (e MachineExport) Namespace() string {
	if e.Metadata.Namespace == "" {
		return "default"
	}
	return e.Metadata.Namespace
}

// ExportTTL parses spec.ttl.
func ExportTTL(s MachineExportSpec) (time.Duration, error) {
	if s.TTL == "" {
		return DefaultExportTTL, nil
	}
	d, err := time.ParseDuration(s.TTL)
	if err != nil {
		return 0, fmt.Errorf("spec.ttl: %w", err)
	}
	if d <= 0 || d > MaxExportTTL {
		return 0, fmt.Errorf("spec.ttl must be between 1s and %s", MaxExportTTL)
	}
	return d, nil
}

// ValidateMachineExportSpec checks the fields the CRD schema can't.
func ValidateMachineExportSpec(s MachineExportSpec) error {
	if s.MachineName == "" {
		return fmt.Errorf("spec.machineName is required")
	}
	switch s.Format {
	case "", "qcow2", "raw":
	default:
		return fmt.Errorf("spec.format must be qcow2 or raw")
	}
	_, err := ExportTTL(s)
	return err
}

// ExportDownloadPath is the kairon-ui route that streams a Ready export.
func ExportDownloadPath(namespace, name string) string {
	return "/api/v1/exports/" + namespace + "/" + name + "/download"
}

// ExportTerminal reports whether an export phase never changes again.
func ExportTerminal(phase string) bool {
	return phase == "Expired" || phase == "Failed"
}
