// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strings"
	"time"
)

// PowerStateHibernated saves the guest's RAM, CPU and device state through
// FluxVM, stops the VMM and frees the node's CPU and memory. Running resumes
// the same boot. See docs/guides/machine-hibernate.md.
const PowerStateHibernated = "Hibernated"

// AnnotationDiscardHibernation set to "true" on a hibernated Machine drops
// its saved state: the next Running boots from disk instead of resuming.
const AnnotationDiscardHibernation = "kairon.zyvor.dev/discard-hibernation"

// PowerStateFreesCapacity reports whether a Machine in desired power state
// p holds no VMM on its node, so neither node capacity nor MachineQuota
// counts it.
func PowerStateFreesCapacity(p string) bool {
	return p == "Stopped" || p == "Halted" || p == PowerStateHibernated
}

// HibernationHostFacts are the host properties a saved QEMU state depends
// on, mirrored from FluxVM's GET /v1/host/hibernation.
type HibernationHostFacts struct {
	QEMUVersion string   `json:"qemuVersion,omitempty"`
	MachineType string   `json:"machineType,omitempty"`
	CPUVendor   string   `json:"cpuVendor,omitempty"`
	CPUFlags    []string `json:"cpuFlags,omitempty"`
	// StateDir is FluxVM's hibernation directory; another node can only
	// import a state from the same shared path.
	StateDir string `json:"stateDir,omitempty"`
}

// HibernationStatus is set while a Machine's guest state is saved.
type HibernationStatus struct {
	// Node hibernated the Machine and holds its FluxVM record.
	Node      string `json:"node"`
	RuntimeID string `json:"runtimeID"`
	// StateBytes is the size of the saved state.
	StateBytes   int64      `json:"stateBytes,omitempty"`
	StateDir     string     `json:"stateDir,omitempty"`
	HibernatedAt *time.Time `json:"hibernatedAt,omitempty"`
	// SharedStorage is true when the root disk is on shared storage, the
	// precondition for resuming on another node.
	SharedStorage bool                 `json:"sharedStorage,omitempty"`
	Host          HibernationHostFacts `json:"host"`
	// ResumeNode is set by kairon-controller once the Machine is wanted
	// Running again: the node whose agent resumes it.
	ResumeNode string `json:"resumeNode,omitempty"`
	// Message explains why a resume is waiting.
	Message string `json:"message,omitempty"`
}

// HibernationIncompatibilities lists why a state saved on a host with
// saved's facts cannot be resumed on a host with host's; empty when it can.
// FluxVM repeats the same check before it loads a state.
func HibernationIncompatibilities(saved, host HibernationHostFacts) []string {
	var out []string
	if saved.QEMUVersion != host.QEMUVersion {
		out = append(out, fmt.Sprintf("QEMU %s saved the state, node has %s", saved.QEMUVersion, host.QEMUVersion))
	}
	if saved.MachineType != host.MachineType {
		out = append(out, fmt.Sprintf("machine type %s saved, node has %s", saved.MachineType, host.MachineType))
	}
	if saved.CPUVendor != host.CPUVendor {
		out = append(out, fmt.Sprintf("CPU vendor %s saved, node is %s", saved.CPUVendor, host.CPUVendor))
	}
	have := make(map[string]bool, len(host.CPUFlags))
	for _, f := range host.CPUFlags {
		have[f] = true
	}
	var missing []string
	for _, f := range saved.CPUFlags {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		out = append(out, "CPU lacks flags: "+strings.Join(missing, " "))
	}
	if saved.StateDir != host.StateDir {
		out = append(out, fmt.Sprintf("hibernation directory %s saved, node uses %s", saved.StateDir, host.StateDir))
	}
	return out
}
