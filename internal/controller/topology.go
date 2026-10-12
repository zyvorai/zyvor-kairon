// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
	"github.com/zyvorai/kairon/internal/topology"
)

// nodeTopologies lists every NodeTopology by node name and refreshes the
// per-NUMA free CPU gauge. A cluster without the CRD yields an empty map.
func (c *Controller) nodeTopologies(ctx context.Context, nodes []model.Node, machines []model.Machine) (map[string]model.NodeTopology, error) {
	list, err := c.Kube.ListNodeTopologies(ctx)
	if err != nil {
		if kube.IsNotFound(err) {
			return map[string]model.NodeTopology{}, nil
		}
		return nil, err
	}
	out := make(map[string]model.NodeTopology, len(list))
	for _, t := range list {
		out[t.Metadata.Name] = t
	}
	if c.Metrics != nil {
		free := map[string]map[int]int{}
		gpus := map[string]metrics.NodeGPUs{}
		for _, n := range nodes {
			nt, ok := out[n.Metadata.Name]
			if !ok {
				continue
			}
			g := topology.BuildGraph(nt, n, machines, model.Machine{})
			byNUMA := map[int]int{}
			for _, numa := range g.NUMANodes {
				byNUMA[numa.ID] = len(numa.FreeCPUs)
			}
			free[n.Metadata.Name] = byNUMA
			if ng, ok := nodeGPUs(nt, n.Metadata.Name, machines); ok {
				gpus[n.Metadata.Name] = ng
			}
		}
		c.Metrics.ObserveNUMAFreeCPUs(free)
		c.Metrics.ObserveGPUs(gpus)
	}
	return out, nil
}

// nodeGPUs summarizes a NodeTopology's GPUs for metrics; ok is false when
// the node has none.
func nodeGPUs(nt model.NodeTopology, node string, machines []model.Machine) (metrics.NodeGPUs, bool) {
	g := metrics.NodeGPUs{ByHealth: map[string]int{}, VGPUAvailable: map[[2]string]int{}}
	health := func(h string) string {
		if h == "" {
			return "Unknown"
		}
		return h
	}
	parents := map[string]bool{}
	for _, p := range nt.Status.MediatedDevices {
		parents[p.Parent] = true
		g.ByHealth[health(p.Health)]++
		for _, t := range p.Types {
			g.VGPUAvailable[[2]string{p.Parent, t.ID}] = t.Available
		}
	}
	for _, d := range nt.Status.Devices {
		if d.Class == model.DeviceClassGPU && !parents[d.BDF] {
			g.ByHealth[health(d.Health)]++
		}
	}
	if len(g.ByHealth) == 0 {
		return g, false
	}
	v := topology.VRAMUsage(nt, node, machines)
	g.VRAMTotalMiB, g.VRAMAllocatedMiB = v.TotalMiB, v.AllocatedMiB
	return g, true
}

// unschedulableReason is the Scheduled condition reason for a Choose
// failure.
func unschedulableReason(m model.Machine, err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "DRAAllocationPending"):
		return "DRAAllocationPending"
	case m.Spec.NeedsTopology() && strings.Contains(msg, scheduler.TopologyUnsatisfiable):
		return scheduler.TopologyUnsatisfiable
	}
	return "Unschedulable"
}

func (c *Controller) observeTopologyFailure(m model.Machine, reason string) {
	if c.Metrics != nil && m.Spec.NeedsTopology() {
		c.Metrics.ObserveTopologyFailure(reason)
	}
}

// liveTopologyTarget places the Machine's running guest NUMA shape on the
// live migration target. It returns nil when the Machine has no NUMA
// allocation to preserve (policy none).
func (c *Controller) liveTopologyTarget(ctx context.Context, m model.Machine, target string, nodes []model.Node, machines []model.Machine) (*topology.Allocation, error) {
	if m.Spec.TopologyAllocation == nil || len(m.Spec.TopologyAllocation.NUMANodes) == 0 {
		return nil, nil
	}
	nt, n, err := c.targetTopology(ctx, target, nodes)
	if err != nil {
		return nil, err
	}
	req, err := topology.RequestFor(m)
	if err != nil {
		return nil, err
	}
	g := topology.BuildGraph(nt, n, machines, m)
	alloc, reason := topology.AllocateShape(g, m.Spec.TopologyAllocation.NUMANodes, req.HugepageSize)
	if reason != "" {
		return nil, fmt.Errorf("migration preflight failed: TopologyShapeUnavailable: %s: %s", target, reason)
	}
	return &alloc, nil
}

// coldTopologyTarget re-allocates the stopped Machine on the cold
// migration target and adds the allocation to the Machine spec patch.
func (c *Controller) coldTopologyTarget(ctx context.Context, m model.Machine, target string, nodes []model.Node, machines []model.Machine, spec map[string]any) error {
	nt, n, err := c.targetTopology(ctx, target, nodes)
	if err != nil {
		if !kube.IsNotFound(errors.Unwrap(err)) || m.Spec.TopologyPolicy() != model.TopologyPolicyBestEffort || len(m.Spec.Devices) > 0 {
			return err
		}
		spec["topologyAllocation"] = nil
		if m.Spec.Resources.CPUPinning {
			cpus, err := scheduler.AllocateCPUSet(m, n, machines)
			if err != nil {
				return fmt.Errorf("allocate CPUs on %s: %w; retrying", target, err)
			}
			spec["resources"] = map[string]any{"allocatedCpuSet": cpus}
		}
		return nil
	}
	alloc, _, reason, _ := scheduler.AllocateTopology(m, n, machines, scheduler.TopologyInput{Nodes: map[string]model.NodeTopology{target: nt}})
	if reason != "" {
		return fmt.Errorf("%s on %s: %s; retrying", scheduler.TopologyUnsatisfiable, target, reason)
	}
	spec["topologyAllocation"] = alloc.TopologyAllocation()
	if alloc.CPUs != nil {
		spec["resources"] = map[string]any{"allocatedCpuSet": alloc.CPUs}
	}
	return nil
}

func (c *Controller) targetTopology(ctx context.Context, target string, nodes []model.Node) (model.NodeTopology, model.Node, error) {
	n, ok := nodeByName(nodes, target)
	if !ok {
		return model.NodeTopology{}, model.Node{}, fmt.Errorf("target node %s not found", target)
	}
	nt, err := c.Kube.GetNodeTopology(ctx, target)
	if err != nil {
		return model.NodeTopology{}, n, fmt.Errorf("migration preflight failed: TopologyShapeUnavailable: NodeTopology %s: %w", target, err)
	}
	return nt, n, nil
}

// markScheduled records a placement made this tick in machines, so later
// Machines in the same tick see its cores, NUMA memory and devices taken.
func markScheduled(machines []model.Machine, m model.Machine, node string, cpus []uint32, alloc *model.TopologyAllocation) {
	for i := range machines {
		if machines[i].Namespace() != m.Namespace() || machines[i].Metadata.Name != m.Metadata.Name {
			continue
		}
		machines[i].Spec.NodeName = node
		if cpus != nil {
			machines[i].Spec.Resources.AllocatedCPUSet = cpus
		}
		if alloc != nil {
			machines[i].Spec.TopologyAllocation = alloc
		}
		return
	}
}
