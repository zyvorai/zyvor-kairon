// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package hotplugpersist keeps a Machine's realized CPU and memory across
// stop/start. FluxVM boots from the create request, not from QMP hotplug
// state, so a restart used to come back at spec.resources even when
// status.applied* was larger. Boot reports the size to create with.
// SpecPatch writes that size back only when the Machine opted in.
package hotplugpersist

import (
	"fmt"
	"strconv"

	"github.com/zyvorai/kairon/internal/model"
)

// AnnPersist opts a Machine into writing the realized size back to
// spec.resources. Without it, stop/start still boots at the realized
// size, but spec stays at whatever GitOps set.
const AnnPersist = "kairon.zyvor.dev/hotplug-persist"

// Size is the boot size and the spec strings that encode it.
type Size struct {
	VCPUs     uint32
	MemoryMiB uint64
	CPU       string
	Memory    string
}

// Boot is max(spec, last applied), clamped to maxCpu/maxMemory when those
// are set. A zero applied value means there is no realized size yet.
func Boot(res model.ResourceSpec, appliedVCPUs uint32, appliedMemoryMiB uint64) (Size, error) {
	specCPU, err := model.ParseVCPUs(res.CPU)
	if err != nil {
		return Size{}, fmt.Errorf("spec.resources.cpu: %w", err)
	}
	specMem, err := model.ParseMemoryMiB(res.Memory)
	if err != nil {
		return Size{}, fmt.Errorf("spec.resources.memory: %w", err)
	}
	cpu := specCPU
	if appliedVCPUs > cpu {
		cpu = appliedVCPUs
	}
	mem := specMem
	if appliedMemoryMiB > mem {
		mem = appliedMemoryMiB
	}
	if res.MaxCPU != "" {
		maxCPU, err := model.ParseVCPUs(res.MaxCPU)
		if err != nil {
			return Size{}, fmt.Errorf("spec.resources.maxCpu: %w", err)
		}
		if cpu > maxCPU {
			cpu = maxCPU
		}
	}
	if res.MaxMemory != "" {
		maxMem, err := model.ParseMemoryMiB(res.MaxMemory)
		if err != nil {
			return Size{}, fmt.Errorf("spec.resources.maxMemory: %w", err)
		}
		if mem > maxMem {
			mem = maxMem
		}
	}
	return Size{VCPUs: cpu, MemoryMiB: mem, CPU: strconv.FormatUint(uint64(cpu), 10), Memory: fmt.Sprintf("%dMi", mem)}, nil
}

// WantsSpecWrite reports whether the controller-side writeback is requested.
func WantsSpecWrite(annotations map[string]string) bool {
	return annotations[AnnPersist] == "true"
}

// SpecPatch is a merge-patch that raises spec.resources.cpu/memory to the
// realized size. Nil when spec is already at least that large, or when
// persist is off. It never shrinks.
func SpecPatch(m model.Machine, appliedVCPUs uint32, appliedMemoryMiB uint64) map[string]any {
	if !WantsSpecWrite(m.Metadata.Annotations) {
		return nil
	}
	size, err := Boot(m.Spec.Resources, appliedVCPUs, appliedMemoryMiB)
	if err != nil {
		return nil
	}
	specCPU, err := model.ParseVCPUs(m.Spec.Resources.CPU)
	if err != nil {
		return nil
	}
	specMem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
	if err != nil {
		return nil
	}
	if size.VCPUs <= specCPU && size.MemoryMiB <= specMem {
		return nil
	}
	cpu, mem := m.Spec.Resources.CPU, m.Spec.Resources.Memory
	if size.VCPUs > specCPU {
		cpu = size.CPU
	}
	if size.MemoryMiB > specMem {
		mem = size.Memory
	}
	return map[string]any{
		"spec": map[string]any{
			"resources": map[string]any{"cpu": cpu, "memory": mem},
		},
	}
}
