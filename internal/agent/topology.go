// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	topologyEvery   = time.Minute
	topologyRefresh = 10 * time.Minute
)

type topologyState struct {
	mu        sync.Mutex
	at        time.Time
	seen      bool
	published model.NodeTopologyStatus
	pubAt     time.Time
}

// publishNodeTopology mirrors FluxVM's GET /v1/host/topology into this
// node's NodeTopology. It checks once a minute and writes when the graph
// changed (free memory and free hugepages aside) or every ten minutes.
func (a *Agent) publishNodeTopology(ctx context.Context) {
	if a.Kube == nil || a.Flux == nil || a.NodeName == "" {
		return
	}
	s := &a.topology
	s.mu.Lock()
	due := !s.seen || time.Since(s.at) >= topologyEvery
	if due {
		s.at, s.seen = time.Now(), true
	}
	s.mu.Unlock()
	if !due {
		return
	}
	caps, err := a.Flux.GetRuntimeCapabilities(ctx)
	if err != nil || !caps.Host.Topology {
		return
	}
	host, err := a.Flux.GetHostTopology(ctx)
	if err != nil {
		a.log().Debug("FluxVM host topology read failed", "error", err)
		return
	}
	status := fluxvm.NodeTopologyStatus(host, caps.Host.NUMABinding, a.VFIOAllowlist)

	s.mu.Lock()
	unchanged := s.pubAt != (time.Time{}) && sameTopology(s.published, status) && time.Since(s.pubAt) < topologyRefresh
	s.mu.Unlock()
	if unchanged {
		return
	}
	now := time.Now().UTC().Truncate(time.Second)
	status.ObservedAt = &now
	if err := a.writeNodeTopology(ctx, status); err != nil {
		a.log().Warn("NodeTopology publish failed", "node", a.NodeName, "error", err)
		return
	}
	s.mu.Lock()
	s.published, s.pubAt = status, time.Now()
	s.mu.Unlock()
}

func (a *Agent) writeNodeTopology(ctx context.Context, status model.NodeTopologyStatus) error {
	if _, err := a.Kube.GetNodeTopology(ctx, a.NodeName); err != nil {
		if !kube.IsNotFound(err) {
			return err
		}
		nt := model.NodeTopology{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindNodeTopology},
			Metadata: model.ObjectMeta{Name: a.NodeName, Labels: map[string]string{"kubernetes.io/hostname": a.NodeName}},
		}
		if _, err := a.Kube.CreateNodeTopology(ctx, nt); err != nil && !kube.IsConflict(err) {
			return err
		}
	}
	return a.Kube.PatchNodeTopologyStatus(ctx, a.NodeName, status)
}

// sameTopology compares two statuses without the fields that move all the
// time and that the scheduler does not read.
func sameTopology(a, b model.NodeTopologyStatus) bool {
	strip := func(s model.NodeTopologyStatus) model.NodeTopologyStatus {
		s.ObservedAt = nil
		nodes := make([]model.TopologyNUMANode, len(s.NUMANodes))
		for i, n := range s.NUMANodes {
			n.FreeMemoryMiB = 0
			hp := make([]model.TopologyHugepages, len(n.Hugepages))
			for j, h := range n.Hugepages {
				h.Free = 0
				hp[j] = h
			}
			n.Hugepages = hp
			nodes[i] = n
		}
		s.NUMANodes = nodes
		return s
	}
	return reflect.DeepEqual(strip(a), strip(b))
}

// topologyVFIODevices returns the BDFs of spec.topologyAllocation's
// devices, bound to vfio-pci. Each must be in this node's allowlist.
func (a *Agent) topologyVFIODevices(ctx context.Context, m model.Machine) ([]string, error) {
	if m.Spec.TopologyAllocation == nil || len(m.Spec.TopologyAllocation.Devices) == 0 {
		return nil, nil
	}
	var out []string
	for _, d := range m.Spec.TopologyAllocation.Devices {
		bdf, err := NormalizeBDF(d.BDF)
		if err != nil {
			return nil, fmt.Errorf("spec.topologyAllocation device %s: %w", d.Name, err)
		}
		if _, ok := a.VFIOAllowlist[bdf]; !ok {
			return nil, fmt.Errorf("device %s (%s) is not in this node's VFIO allowlist", bdf, d.Name)
		}
		if err := a.Flux.BindHostPCI(ctx, bdf); err != nil {
			return nil, fmt.Errorf("bind %s (%s) to vfio-pci: %w", bdf, d.Name, err)
		}
		out = append(out, bdf)
	}
	return out, nil
}

func mergeBDFs(a, b []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, bdf := range append(append([]string{}, a...), b...) {
		if _, ok := seen[bdf]; !ok {
			seen[bdf] = struct{}{}
			out = append(out, bdf)
		}
	}
	sort.Strings(out)
	return out
}

// appliedTopology reads the VM's live NUMA placement from FluxVM and
// notes where it differs from spec.topologyAllocation. It returns the
// previous value when FluxVM cannot be read.
func (a *Agent) appliedTopology(ctx context.Context, m model.Machine, rec *fluxvm.Record, vfio []string) *model.AppliedTopology {
	alloc := m.Spec.TopologyAllocation
	if alloc == nil || rec == nil {
		return nil
	}
	if len(alloc.NUMANodes) == 0 {
		return &model.AppliedTopology{Devices: vfio}
	}
	st, err := a.Flux.GetVMNUMA(ctx, rec.ID())
	if err != nil {
		a.log().Debug("FluxVM VM NUMA read failed", "runtimeID", rec.ID(), "error", err)
		return m.Status.AppliedTopology
	}
	return buildAppliedTopology(alloc, st, vfio)
}

func buildAppliedTopology(alloc *model.TopologyAllocation, st *fluxvm.VMNUMAStatus, vfio []string) *model.AppliedTopology {
	out := &model.AppliedTopology{Devices: vfio}
	policy := map[int]string{}
	for _, md := range st.Memdevs {
		if len(md.HostNodes) == 1 {
			policy[md.HostNodes[0]] = md.Policy
		}
	}
	hostCPUs := map[int]map[uint32]struct{}{}
	for _, v := range st.VCPUs {
		if hostCPUs[v.HostNode] == nil {
			hostCPUs[v.HostNode] = map[uint32]struct{}{}
		}
		for _, c := range v.HostCPUs {
			hostCPUs[v.HostNode][c] = struct{}{}
		}
	}
	for _, n := range st.Nodes {
		cpus := make([]uint32, 0, len(hostCPUs[n.HostNode]))
		for c := range hostCPUs[n.HostNode] {
			cpus = append(cpus, c)
		}
		sort.Slice(cpus, func(i, j int) bool { return cpus[i] < cpus[j] })
		out.NUMANodes = append(out.NUMANodes, model.AppliedNUMANode{
			HostNode: n.HostNode, VCPUs: model.FormatCPUList(n.VCPUs), HostCPUs: model.FormatCPUList(cpus),
			MemoryMiB: n.MemoryMiB, Policy: policy[n.HostNode],
		})
	}
	if len(out.NUMANodes) != len(alloc.NUMANodes) {
		out.Message = fmt.Sprintf("FluxVM reports %d guest NUMA nodes, spec.topologyAllocation has %d", len(out.NUMANodes), len(alloc.NUMANodes))
		return out
	}
	for i, n := range out.NUMANodes {
		want := alloc.NUMANodes[i]
		switch {
		case n.HostNode != want.ID:
			out.Message = fmt.Sprintf("guest node %d is on host node %d, allocated %d", i, n.HostNode, want.ID)
		case n.Policy != "bind":
			out.Message = fmt.Sprintf("guest node %d memory policy is %q, not bind", i, n.Policy)
		case n.HostCPUs != want.CPUs:
			out.Message = fmt.Sprintf("guest node %d vCPU threads run on %q, allocated %q (vCPU pinning is best-effort)", i, n.HostCPUs, want.CPUs)
		default:
			continue
		}
		break
	}
	return out
}
