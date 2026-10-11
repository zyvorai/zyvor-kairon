// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// NodeLoad tracks per-node placement pressure for one reconcile pass:
// Machine count (legacy signal) plus requested CPU/memory so simultaneous
// placements cannot overcommit a node's allocatable capacity.
type NodeLoad struct {
	Count     int
	CPUMilli  int64
	MemoryMiB uint64
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
		cpu, mem := machineRequests(m)
		l := out[m.Spec.NodeName]
		l.Count++
		l.CPUMilli += cpu
		l.MemoryMiB += mem
		out[m.Spec.NodeName] = l
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
	if reqMem > 0 && load.MemoryMiB+reqMem > allocMem {
		return false, fmt.Sprintf("insufficient memory: need %dMi, allocatable %dMi with %dMi already assigned", reqMem, allocMem, load.MemoryMiB)
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
// back to -Count so least-loaded-by-count behavior is preserved.
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
		var rem uint64
		used := load.MemoryMiB + reqMem
		if used < allocMem {
			rem = allocMem - used
		}
		memPct = int(rem * 100 / allocMem)
	}
	return (cpuPct + memPct) / 2
}

// Reserve records that m was just placed on nodeName within this reconcile
// pass so later Choose calls see the reserved capacity.
func Reserve(load map[string]NodeLoad, nodeName string, m model.Machine) {
	cpu, mem := machineRequests(m)
	l := load[nodeName]
	l.Count++
	l.CPUMilli += cpu
	l.MemoryMiB += mem
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
