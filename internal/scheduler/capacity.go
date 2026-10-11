// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"
	"strconv"

	"github.com/zyvorai/kairon/internal/model"
)

// MaxMemoryOvercommitRatio caps model.AnnotationMemoryOvercommitRatio.
const MaxMemoryOvercommitRatio = 4.0

// NodeLoad tracks per-node placement pressure for one reconcile pass:
// Machine count (legacy signal) plus requested CPU/memory so simultaneous
// placements cannot overcommit a node's allocatable capacity.
type NodeLoad struct {
	Count     int
	CPUMilli  int64
	MemoryMiB uint64
	// GuaranteedMemoryMiB counts elastic-memory Machines at their minimum
	// and every other Machine at its full memory.
	GuaranteedMemoryMiB uint64
}

// BuildNodeLoad sums resource requests of Machines that currently consume
// node capacity (Running/Paused with a nodeName), matching countAssigned.
func BuildNodeLoad(machines []model.Machine) map[string]NodeLoad {
	out := map[string]NodeLoad{}
	for _, m := range machines {
		desired := m.DesiredPowerState()
		if m.Spec.NodeName == "" || m.Metadata.DeletionTimestamp != nil || (desired != "Running" && desired != "Paused") {
			continue
		}
		Reserve(out, m.Spec.NodeName, m)
	}
	return out
}

func machineRequests(m model.Machine) (cpuMilli int64, memoryMiB uint64) {
	if vcpus, err := model.ParseVCPUs(m.Spec.Resources.CPU); err == nil {
		cpuMilli = int64(vcpus) * 1000
	}
	if mem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory); err == nil {
		memoryMiB = mem
	}
	return cpuMilli, memoryMiB
}

// guaranteedMemoryMiB is the memory m keeps under host pressure: its
// elastic minimum, or all of memoryMiB.
func guaranteedMemoryMiB(m model.Machine, memoryMiB uint64) uint64 {
	if min, ok := m.Spec.ElasticMinMiB(); ok && min < memoryMiB {
		return min
	}
	return memoryMiB
}

// MemoryOvercommitRatio is the node's model.AnnotationMemoryOvercommitRatio,
// clamped to 1.0..MaxMemoryOvercommitRatio; 1.0 when unset or invalid.
func MemoryOvercommitRatio(n model.Node) float64 {
	raw, ok := n.Metadata.Annotations[model.AnnotationMemoryOvercommitRatio]
	if !ok {
		return 1
	}
	r, err := strconv.ParseFloat(raw, 64)
	if err != nil || r < 1 {
		return 1
	}
	if r > MaxMemoryOvercommitRatio {
		return MaxMemoryOvercommitRatio
	}
	return r
}

func nodeAllocatable(n model.Node) (cpuMilli int64, memoryMiB uint64, ok bool) {
	if n.Status.Allocatable == nil {
		return 0, 0, false
	}
	cpuRaw, hasCPU := n.Status.Allocatable["cpu"]
	memRaw, hasMem := n.Status.Allocatable["memory"]
	if !hasCPU || !hasMem {
		return 0, 0, false
	}
	vcpus, err := model.ParseVCPUs(cpuRaw)
	if err != nil {
		return 0, 0, false
	}
	mem, err := model.ParseMemoryMiB(memRaw)
	if err != nil {
		return 0, 0, false
	}
	return int64(vcpus) * 1000, mem, true
}

func capacityFits(n model.Node, m model.Machine, load NodeLoad) (bool, string) {
	allocCPU, allocMem, ok := nodeAllocatable(n)
	if !ok {
		// Nodes without allocatable quantities stay eligible for capacity
		// purposes (tests, incomplete Node objects); scoring falls back
		// to Machine count for those nodes.
		return true, ""
	}
	reqCPU, reqMem := machineRequests(m)
	if reqCPU > 0 && load.CPUMilli+reqCPU > allocCPU {
		return false, fmt.Sprintf("insufficient CPU: need %dm, allocatable %dm with %dm already assigned", reqCPU, allocCPU, load.CPUMilli)
	}
	if reqMem > 0 {
		ratio := MemoryOvercommitRatio(n)
		if ratio == 1 {
			if load.MemoryMiB+reqMem > allocMem {
				return false, fmt.Sprintf("insufficient memory: need %dMi, allocatable %dMi with %dMi already assigned", reqMem, allocMem, load.MemoryMiB)
			}
		} else {
			reqMin := guaranteedMemoryMiB(m, reqMem)
			if load.GuaranteedMemoryMiB+reqMin > allocMem {
				return false, fmt.Sprintf("insufficient guaranteed memory: need %dMi, allocatable %dMi with %dMi already guaranteed", reqMin, allocMem, load.GuaranteedMemoryMiB)
			}
			limit := uint64(float64(allocMem) * ratio)
			if load.MemoryMiB+reqMem > limit {
				return false, fmt.Sprintf("insufficient memory: need %dMi, overcommit limit %dMi (%.2gx allocatable) with %dMi already assigned", reqMem, limit, ratio, load.MemoryMiB)
			}
		}
	}
	if m.Spec.Resources.Hugepages {
		if hp, has := n.Status.Allocatable["hugepages-2Mi"]; has {
			if hp == "0" || hp == "0Mi" {
				return false, "hugepages requested but node allocatable hugepages-2Mi is 0"
			}
		}
	}
	return true, ""
}

// remainingCapacityScore returns 0..100 based on post-placement remaining
// CPU and memory percentage (average). When allocatable is unknown, falls
// back to -Count so least-loaded-by-count behavior is preserved. Memory is
// the tighter of guaranteed memory against allocatable and full memory
// against the overcommit limit.
func remainingCapacityScore(n model.Node, m model.Machine, load NodeLoad) int {
	allocCPU, allocMem, ok := nodeAllocatable(n)
	if !ok || (allocCPU <= 0 && allocMem <= 0) {
		return -load.Count
	}
	reqCPU, reqMem := machineRequests(m)
	cpuPct, memPct := 100, 100
	if allocCPU > 0 {
		rem := allocCPU - load.CPUMilli - reqCPU
		if rem < 0 {
			rem = 0
		}
		cpuPct = int(rem * 100 / allocCPU)
	}
	if allocMem > 0 {
		limit := uint64(float64(allocMem) * MemoryOvercommitRatio(n))
		memPct = remainingPercent(load.MemoryMiB+reqMem, limit)
		if g := remainingPercent(load.GuaranteedMemoryMiB+guaranteedMemoryMiB(m, reqMem), allocMem); g < memPct {
			memPct = g
		}
	}
	return (cpuPct + memPct) / 2
}

func remainingPercent(used, total uint64) int {
	if total == 0 || used >= total {
		return 0
	}
	return int((total - used) * 100 / total)
}

// Reserve records that m was just placed on nodeName within this reconcile
// pass so later Choose calls see the reserved capacity.
func Reserve(load map[string]NodeLoad, nodeName string, m model.Machine) {
	cpu, mem := machineRequests(m)
	l := load[nodeName]
	l.Count++
	l.CPUMilli += cpu
	l.MemoryMiB += mem
	l.GuaranteedMemoryMiB += guaranteedMemoryMiB(m, mem)
	load[nodeName] = l
}

// LoadFromCounts builds a NodeLoad map from Machine counts only. Used by
// tests that exercise affinity/taints without capacity accounting.
func LoadFromCounts(counts map[string]int) map[string]NodeLoad {
	out := make(map[string]NodeLoad, len(counts))
	for k, v := range counts {
		out[k] = NodeLoad{Count: v}
	}
	return out
}

// Allocatable reports a Node's allocatable CPU (millicores) and memory
// (MiB); ok is false when the Node object carries no quantities.
func Allocatable(n model.Node) (cpuMilli int64, memoryMiB uint64, ok bool) {
	return nodeAllocatable(n)
}

// Fits reports whether m's CPU/memory/hugepages requests fit n on top of
// load, with the human reason when they do not -- the same hard filter
// Choose applies, exposed for the dashboard's capacity what-if.
func Fits(n model.Node, m model.Machine, load NodeLoad) (bool, string) {
	return capacityFits(n, m, load)
}
