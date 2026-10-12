// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package topology places a Machine's CPUs, memory and PCI devices on a
// node's NUMA nodes and PCIe roots, from the node's NodeTopology and the
// allocations other Machines already hold. Allocate is deterministic: the
// same graph and request always give the same answer.
package topology

import (
	"crypto/sha1" //nolint:gosec // a name-based UUID, not a security digest
	"fmt"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// NUMANode is one host NUMA node's capacity left for a new Machine.
type NUMANode struct {
	ID       int
	FreeCPUs []uint32
	// FreeMemoryMiB is the node's memory minus other Machines'
	// topologyAllocation on it.
	FreeMemoryMiB int64
	// FreeHugepages is free pages by size ("2Mi", "1Gi") minus other
	// Machines' allocations.
	FreeHugepages map[string]int64
}

// Device is an allocatable PCI device nobody holds yet.
type Device struct {
	model.TopologyDevice
	// NUMA is the device's NUMA node, or -1 when the kernel does not say.
	NUMA int
}

// MediatedParent is an allocatable, not Unhealthy vGPU parent.
type MediatedParent struct {
	model.TopologyMediatedParent
	// NUMA is the parent's NUMA node, or -1 when unknown.
	NUMA int
	// PendingMiB and Pending (by type) are instances other Machines are
	// allocated that the parent does not list yet, so its available
	// counts do not include them.
	PendingMiB int64
	Pending    map[string]int
}

// Free is how many more instances of t fit on p after pending ones and
// extraMiB of framebuffer / extraN instances of t picked by this request.
// With a known framebuffer the parent is treated as one memory pool, which
// is exact for the kernel's mbochs and NVIDIA's mixed-size vGPU, and
// optimistic for parents that allow one profile at a time (FluxVM's
// create is the final word there).
func (p MediatedParent) Free(t model.TopologyMediatedType, extraMiB int64, extraN int) int {
	var n int64
	if t.FramebufferMiB > 0 {
		n = (int64(t.Available)*t.FramebufferMiB - p.PendingMiB - extraMiB) / t.FramebufferMiB
	} else {
		n = int64(t.Available - p.Pending[t.ID] - extraN)
	}
	if n < 0 {
		return 0
	}
	return int(n)
}

// PCI reports whether the parent is a PCI device (Parent is its BDF).
func (p MediatedParent) PCI() bool { return p.Vendor != "" }

type Graph struct {
	NUMANodes []NUMANode
	Devices   []Device
	Mediated  []MediatedParent
	// Binding is true when FluxVM on the node binds guest NUMA memory.
	Binding bool
}

type Request struct {
	VCPUs        int
	MemoryMiB    int64
	HugepageSize string
	Policy       string
	Devices      []model.MachineDevice
	// RequiredNUMA are host NUMA nodes the Machine's CPUs must include,
	// from DRA-allocated devices.
	RequiredNUMA []int
	// Seed makes mediated device UUIDs stable for one Machine: its UID,
	// or namespace/name.
	Seed string
}

type Allocation struct {
	NUMANodes []model.AllocatedNUMANode
	Devices   []model.AllocatedDevice
	// CPUs is every host CPU the Machine owns, ascending.
	CPUs []uint32
}

// TopologyAllocation is the part patched into spec.topologyAllocation.
func (a Allocation) TopologyAllocation() *model.TopologyAllocation {
	return &model.TopologyAllocation{NUMANodes: a.NUMANodes, Devices: a.Devices}
}

func hugepageMiB(size string) int64 {
	switch size {
	case "2Mi":
		return 2
	case "1Gi":
		return 1024
	}
	return 0
}

// RequestFor turns a Machine's spec into a Request.
func RequestFor(m model.Machine) (Request, error) {
	vcpus, err := model.ParseVCPUs(m.Spec.Resources.CPU)
	if err != nil {
		return Request{}, fmt.Errorf("spec.resources.cpu: %w", err)
	}
	mem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
	if err != nil {
		return Request{}, fmt.Errorf("spec.resources.memory: %w", err)
	}
	r := Request{
		VCPUs:     int(vcpus),
		MemoryMiB: int64(mem),
		Policy:    m.Spec.TopologyPolicy(),
		Devices:   m.Spec.Devices,
		Seed:      m.Metadata.UID,
	}
	if r.Seed == "" {
		r.Seed = m.Namespace() + "/" + m.Metadata.Name
	}
	if m.Spec.Topology != nil {
		r.HugepageSize = m.Spec.Topology.HugepageSize
	}
	return r, nil
}

// BuildGraph is node's NodeTopology minus what every other live Machine on
// it holds: pinned CPUs (allocatedCpuSet), NUMA memory and hugepages
// (topologyAllocation) and devices.
func BuildGraph(nt model.NodeTopology, node model.Node, machines []model.Machine, self model.Machine) Graph {
	pinnable, _ := model.ParseCPUList(node.Metadata.Labels[model.PinnableCPUsLabel])
	pinnableSet := map[uint32]bool{}
	for _, c := range pinnable {
		pinnableSet[c] = true
	}
	usedCPU := map[uint32]bool{}
	usedMem := map[int]int64{}
	usedPages := map[int]map[string]int64{}
	usedBDF := map[string]bool{}
	instances := map[string]bool{}
	mdevParent := map[string]bool{}
	for _, p := range nt.Status.MediatedDevices {
		mdevParent[p.Parent] = true
		for _, u := range p.Instances {
			instances[strings.ToLower(u)] = true
		}
	}
	pendingMiB := map[string]int64{}
	pending := map[string]map[string]int{}
	for _, o := range machines {
		if o.Spec.NodeName != node.Metadata.Name || o.Metadata.DeletionTimestamp != nil {
			continue
		}
		if o.Namespace() == self.Namespace() && o.Metadata.Name == self.Metadata.Name {
			continue
		}
		for _, c := range o.Spec.Resources.AllocatedCPUSet {
			usedCPU[c] = true
		}
		a := o.Spec.TopologyAllocation
		if a == nil {
			continue
		}
		size := ""
		if o.Spec.Topology != nil {
			size = o.Spec.Topology.HugepageSize
		}
		for _, n := range a.NUMANodes {
			if size != "" && n.Hugepages > 0 {
				if usedPages[n.ID] == nil {
					usedPages[n.ID] = map[string]int64{}
				}
				usedPages[n.ID][size] += n.Hugepages
				continue
			}
			usedMem[n.ID] += n.MemoryMiB
		}
		for _, d := range a.Devices {
			if !d.Mediated() {
				usedBDF[d.BDF] = true
				continue
			}
			if instances[strings.ToLower(d.UUID)] {
				continue
			}
			pendingMiB[d.Parent] += d.FramebufferMiB
			if pending[d.Parent] == nil {
				pending[d.Parent] = map[string]int{}
			}
			pending[d.Parent][d.Profile]++
		}
	}

	g := Graph{Binding: nt.Status.NUMABinding}
	for _, n := range nt.Status.NUMANodes {
		cpus, _ := model.ParseCPUList(n.CPUs)
		free := []uint32{}
		for _, c := range cpus {
			if pinnableSet[c] && !usedCPU[c] {
				free = append(free, c)
			}
		}
		pages := map[string]int64{}
		for _, hp := range n.Hugepages {
			pages[hp.Size] = hp.Total - usedPages[n.ID][hp.Size]
		}
		g.NUMANodes = append(g.NUMANodes, NUMANode{
			ID:            n.ID,
			FreeCPUs:      free,
			FreeMemoryMiB: n.MemoryMiB - usedMem[n.ID],
			FreeHugepages: pages,
		})
	}
	sort.Slice(g.NUMANodes, func(i, j int) bool { return g.NUMANodes[i].ID < g.NUMANodes[j].ID })
	numaOf := func(n *int) int {
		switch {
		case n != nil:
			return *n
		case len(g.NUMANodes) == 1:
			return g.NUMANodes[0].ID
		}
		return -1
	}
	// A vGPU parent runs the vendor's host driver, so it cannot also go
	// to a guest whole.
	for _, d := range nt.Status.Devices {
		if !d.Allocatable || d.Allocated || usedBDF[d.BDF] || d.Health == model.DeviceUnhealthy || mdevParent[d.BDF] {
			continue
		}
		g.Devices = append(g.Devices, Device{TopologyDevice: d, NUMA: numaOf(d.NUMANode)})
	}
	sort.Slice(g.Devices, func(i, j int) bool { return g.Devices[i].BDF < g.Devices[j].BDF })
	for _, p := range nt.Status.MediatedDevices {
		if !p.Allocatable || p.Health == model.DeviceUnhealthy {
			continue
		}
		g.Mediated = append(g.Mediated, MediatedParent{
			TopologyMediatedParent: p, NUMA: numaOf(p.NUMANode),
			PendingMiB: pendingMiB[p.Parent], Pending: pending[p.Parent],
		})
	}
	sort.Slice(g.Mediated, func(i, j int) bool { return g.Mediated[i].Parent < g.Mediated[j].Parent })
	return g
}

// NodesForBDFs maps device BDFs to their NUMA nodes in nt; unknown or
// NUMA-less devices are skipped.
func NodesForBDFs(nt model.NodeTopology, bdfs []string) []int {
	want := map[string]bool{}
	for _, b := range bdfs {
		want[strings.ToLower(b)] = true
	}
	seen := map[int]bool{}
	var out []int
	for _, d := range nt.Status.Devices {
		if want[strings.ToLower(d.BDF)] && d.NUMANode != nil && !seen[*d.NUMANode] {
			seen[*d.NUMANode] = true
			out = append(out, *d.NUMANode)
		}
	}
	sort.Ints(out)
	return out
}

// Locality scores, highest first.
const (
	scoreOneNode    = 100
	scoreTwoNodes   = 70
	scoreManyNodes  = 40
	scoreNotLocal   = 20
	scoreNoNUMAPlan = 50
)

// Allocate places r on g. It returns the allocation and a locality score
// (0-100), or a reason when nothing fits. Mediated devices get UUIDs
// derived from r.Seed, so placing the same Machine again names the same
// instances.
func Allocate(g Graph, r Request) (Allocation, int, string) {
	a, score, reason := place(g, r)
	if reason != "" {
		return a, score, reason
	}
	nth := map[string]int{}
	for i, d := range a.Devices {
		if d.Profile == "" {
			continue
		}
		a.Devices[i].UUID = MediatedUUID(r.Seed, d.Name, nth[d.Name])
		nth[d.Name]++
	}
	return a, score, ""
}

// MediatedUUID is the name-based (version 5 layout) UUID of the n-th
// instance of device request name for seed.
func MediatedUUID(seed, name string, n int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("kairon-mdev\x00%s\x00%s\x00%d", seed, name, n)))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func place(g Graph, r Request) (Allocation, int, string) {
	hard := r.Policy == model.TopologyPolicyRestricted || r.Policy == model.TopologyPolicySingleNUMANode
	if hard && !g.Binding {
		return Allocation{}, 0, "the node's FluxVM cannot bind guest NUMA memory"
	}
	if r.Policy == model.TopologyPolicyNone || r.Policy == "" {
		devs, reason := pickDevices(g, r.Devices, nil)
		if reason != "" {
			return Allocation{}, 0, reason
		}
		return Allocation{Devices: devs}, scoreNoNUMAPlan, ""
	}
	if len(g.NUMANodes) == 0 {
		return Allocation{}, 0, "NodeTopology has no NUMA nodes"
	}
	if p := hugepageMiB(r.HugepageSize); p > 0 && r.MemoryMiB%p != 0 {
		return Allocation{}, 0, fmt.Sprintf("memory %dMiB is not a multiple of the %s hugepage size", r.MemoryMiB, r.HugepageSize)
	}

	var sizes []int
	switch r.Policy {
	case model.TopologyPolicySingleNUMANode:
		sizes = []int{1}
	case model.TopologyPolicyRestricted:
		sizes = []int{1, 2}
	default:
		sizes = []int{1, 2, len(g.NUMANodes)}
	}
	var singleReasons []string
	tried := map[int]bool{}
	for _, size := range sizes {
		if size > len(g.NUMANodes) || tried[size] {
			continue
		}
		tried[size] = true
		var best *Allocation
		bestSlack := 0
		for _, combo := range combinations(len(g.NUMANodes), size) {
			a, slack, reason := tryNodes(g, r, combo, true)
			if reason != "" {
				if size == 1 {
					singleReasons = append(singleReasons, fmt.Sprintf("numa%d: %s", g.NUMANodes[combo[0]].ID, reason))
				}
				continue
			}
			if best == nil || slack < bestSlack {
				a := a
				best, bestSlack = &a, slack
			}
		}
		if best != nil {
			switch size {
			case 1:
				return *best, scoreOneNode, ""
			case 2:
				return *best, scoreTwoNodes, ""
			default:
				return *best, scoreManyNodes, ""
			}
		}
	}
	if r.Policy == model.TopologyPolicyBestEffort {
		all := combinations(len(g.NUMANodes), len(g.NUMANodes))[0]
		a, _, reason := tryNodes(g, r, all, false)
		if reason != "" {
			return Allocation{}, 0, reason
		}
		return a, scoreNotLocal, ""
	}
	if r.Policy == model.TopologyPolicySingleNUMANode {
		return Allocation{}, 0, "no single NUMA node fits (" + strings.Join(singleReasons, "; ") + ")"
	}
	return Allocation{}, 0, "no set of one or two NUMA nodes fits (" + strings.Join(singleReasons, "; ") + ")"
}

// AllocateShape places an already-running guest NUMA layout on g for live
// migration: each guest node keeps its CPU count and memory and gets its
// own host NUMA node, the lowest-numbered one with room. Devices are not
// placed (VFIO devices cannot live-migrate).
func AllocateShape(g Graph, shape []model.AllocatedNUMANode, hugepageSize string) (Allocation, string) {
	if !g.Binding {
		return Allocation{}, "the node's FluxVM cannot bind guest NUMA memory"
	}
	used := map[int]bool{}
	var a Allocation
	for gi, s := range shape {
		cpus, err := model.ParseCPUList(s.CPUs)
		if err != nil {
			return Allocation{}, fmt.Sprintf("guest node %d: %v", gi, err)
		}
		placed := false
		for i, n := range g.NUMANodes {
			if used[i] || len(n.FreeCPUs) < len(cpus) {
				continue
			}
			if hugepageSize != "" && s.Hugepages > 0 {
				if n.FreeHugepages[hugepageSize] < s.Hugepages {
					continue
				}
			} else if n.FreeMemoryMiB < s.MemoryMiB {
				continue
			}
			used[i] = true
			take := n.FreeCPUs[:len(cpus)]
			a.CPUs = append(a.CPUs, take...)
			a.NUMANodes = append(a.NUMANodes, model.AllocatedNUMANode{
				ID: n.ID, CPUs: model.FormatCPUList(take), MemoryMiB: s.MemoryMiB, Hugepages: s.Hugepages,
			})
			placed = true
			break
		}
		if !placed {
			return Allocation{}, fmt.Sprintf("no free NUMA node has %d CPUs and %dMiB for guest node %d", len(cpus), s.MemoryMiB, gi)
		}
	}
	sort.Slice(a.CPUs, func(i, j int) bool { return a.CPUs[i] < a.CPUs[j] })
	return a, ""
}

// combinations lists every size-k subset of 0..n-1 in lexicographic order.
func combinations(n, k int) [][]int {
	var out [][]int
	cur := make([]int, 0, k)
	var rec func(start int)
	rec = func(start int) {
		if len(cur) == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := start; i <= n-(k-len(cur)); i++ {
			cur = append(cur, i)
			rec(i + 1)
			cur = cur[:len(cur)-1]
		}
	}
	rec(0)
	return out
}

// tryNodes fits r onto the NUMA nodes at indexes combo. slack is the free
// CPUs left on those nodes, for best-fit packing.
func tryNodes(g Graph, r Request, combo []int, local bool) (Allocation, int, string) {
	inSet := map[int]bool{}
	for _, i := range combo {
		inSet[g.NUMANodes[i].ID] = true
	}
	if local {
		for _, id := range r.RequiredNUMA {
			if !inSet[id] {
				return Allocation{}, 0, fmt.Sprintf("a DRA device is on NUMA node %d", id)
			}
		}
	}

	shares, reason := splitCPUs(g, combo, r.VCPUs)
	if reason != "" {
		return Allocation{}, 0, reason
	}
	page := hugepageMiB(r.HugepageSize)
	var a Allocation
	var assigned int64
	slack := 0
	for k, i := range combo {
		n := g.NUMANodes[i]
		mem := r.MemoryMiB * int64(shares[k]) / int64(r.VCPUs)
		if page > 0 {
			mem -= mem % page
		}
		if k == len(combo)-1 {
			mem = r.MemoryMiB - assigned
		}
		assigned += mem
		an := model.AllocatedNUMANode{ID: n.ID, MemoryMiB: mem}
		if page > 0 {
			an.Hugepages = mem / page
			if free := n.FreeHugepages[r.HugepageSize]; an.Hugepages > free {
				return Allocation{}, 0, fmt.Sprintf("needs %d %s hugepages, %d free", an.Hugepages, r.HugepageSize, free)
			}
		} else if mem > n.FreeMemoryMiB {
			return Allocation{}, 0, fmt.Sprintf("needs %dMiB memory, %dMiB free", mem, n.FreeMemoryMiB)
		}
		cpus := n.FreeCPUs[:shares[k]]
		an.CPUs = model.FormatCPUList(cpus)
		a.CPUs = append(a.CPUs, cpus...)
		a.NUMANodes = append(a.NUMANodes, an)
		slack += len(n.FreeCPUs) - shares[k]
	}
	sort.Slice(a.CPUs, func(i, j int) bool { return a.CPUs[i] < a.CPUs[j] })

	var allowed map[int]bool
	if local {
		allowed = inSet
	}
	devs, reason := pickDevices(g, r.Devices, allowed)
	if reason != "" {
		return Allocation{}, 0, reason
	}
	a.Devices = devs
	return a, slack, ""
}

// splitCPUs spreads vcpus over the nodes at combo as evenly as their free
// CPUs allow, at least one per node.
func splitCPUs(g Graph, combo []int, vcpus int) ([]int, string) {
	free := 0
	for _, i := range combo {
		free += len(g.NUMANodes[i].FreeCPUs)
	}
	if free < vcpus {
		return nil, fmt.Sprintf("%d free pinnable CPUs, needs %d", free, vcpus)
	}
	if vcpus < len(combo) {
		return nil, fmt.Sprintf("%d vCPUs cannot span %d NUMA nodes", vcpus, len(combo))
	}
	shares := make([]int, len(combo))
	for k := range shares {
		shares[k] = vcpus / len(combo)
		if k < vcpus%len(combo) {
			shares[k]++
		}
	}
	over := 0
	for k, i := range combo {
		if c := len(g.NUMANodes[i].FreeCPUs); shares[k] > c {
			over += shares[k] - c
			shares[k] = c
		}
	}
	for k, i := range combo {
		if over == 0 {
			break
		}
		room := len(g.NUMANodes[i].FreeCPUs) - shares[k]
		if room > over {
			room = over
		}
		shares[k] += room
		over -= room
	}
	for _, s := range shares {
		if s == 0 {
			return nil, fmt.Sprintf("%d vCPUs cannot span %d NUMA nodes", vcpus, len(combo))
		}
	}
	return shares, ""
}

func matches(d Device, req model.MachineDevice, minMiB uint64) bool {
	if d.Class != req.Class {
		return false
	}
	if req.VirtualFunction != (d.PhysFn != "") {
		return false
	}
	if req.Vendor != "" && !strings.EqualFold(d.Vendor, model.PCIVendorID(req.Vendor)) {
		return false
	}
	if req.Model != "" && !strings.EqualFold(d.Device, req.Model) {
		return false
	}
	if minMiB > 0 && uint64(d.VRAMGiB)*1024 < minMiB {
		return false
	}
	return true
}

func describe(req model.MachineDevice) string {
	s := fmt.Sprintf("%d %s", req.DeviceCount(), req.Class)
	if req.VirtualFunction {
		s += " VF"
	}
	if req.Profile != "" {
		s += " profile " + req.Profile
	}
	if req.Vendor != "" {
		s += " vendor " + req.Vendor
	}
	if req.Model != "" {
		s += " model " + req.Model
	}
	if req.MinMemory != "" {
		s += " with " + req.MinMemory
	}
	return s
}

// usage is what one allocation has picked so far: whole devices by BDF,
// and mediated framebuffer by parent and instances by parent/type.
type usage struct {
	bdf     map[string]bool
	mdevMiB map[string]int64
	mdevN   map[string]int
}

func newUsage() *usage {
	return &usage{bdf: map[string]bool{}, mdevMiB: map[string]int64{}, mdevN: map[string]int{}}
}

func (u *usage) merge(o *usage) {
	for k := range o.bdf {
		u.bdf[k] = true
	}
	for k, v := range o.mdevMiB {
		u.mdevMiB[k] += v
	}
	for k, v := range o.mdevN {
		u.mdevN[k] += v
	}
}

// pickDevices chooses devices for every request, lowest BDF first. allowed
// limits devices to those NUMA nodes (nil: anywhere). Requests with
// samePcieRoot all share one root.
func pickDevices(g Graph, reqs []model.MachineDevice, allowed map[int]bool) ([]model.AllocatedDevice, string) {
	if len(reqs) == 0 {
		return nil, ""
	}
	var rooted, rest []model.MachineDevice
	for _, r := range reqs {
		if r.Aligned(model.AlignSamePCIeRoot) {
			rooted = append(rooted, r)
		} else {
			rest = append(rest, r)
		}
	}
	used := newUsage()
	var out []model.AllocatedDevice
	if len(rooted) > 0 {
		roots := map[string]bool{}
		for _, d := range g.Devices {
			roots[d.PCIeRoot] = true
		}
		for _, p := range g.Mediated {
			if p.PCIeRoot != "" {
				roots[p.PCIeRoot] = true
			}
		}
		names := make([]string, 0, len(roots))
		for r := range roots {
			names = append(names, r)
		}
		sort.Strings(names)
		found := false
		for _, root := range names {
			tryUsed := newUsage()
			var picked []model.AllocatedDevice
			ok := true
			for _, req := range rooted {
				p, reason := pickOne(g, req, allowed, root, tryUsed)
				if reason != "" {
					ok = false
					break
				}
				picked = append(picked, p...)
			}
			if ok {
				used.merge(tryUsed)
				out = append(out, picked...)
				found = true
				break
			}
		}
		if !found {
			parts := make([]string, 0, len(rooted))
			for _, r := range rooted {
				parts = append(parts, describe(r))
			}
			return nil, "no PCIe root has " + strings.Join(parts, " and ") + " free" + localSuffix(allowed)
		}
	}
	for _, req := range rest {
		p, reason := pickOne(g, req, allowed, "", used)
		if reason != "" {
			return nil, reason
		}
		out = append(out, p...)
	}
	return out, ""
}

func localSuffix(allowed map[int]bool) string {
	if allowed == nil {
		return ""
	}
	return " on the chosen NUMA nodes"
}

func minMemoryMiB(req model.MachineDevice) uint64 {
	if req.MinMemory == "" {
		return 0
	}
	mib, _ := model.ParseMemoryMiB(req.MinMemory)
	return mib
}

// pickOne picks req.Count devices for one request, optionally under one
// PCIe root, all on one NUMA node when sameNuma is set.
func pickOne(g Graph, req model.MachineDevice, allowed map[int]bool, root string, used *usage) ([]model.AllocatedDevice, string) {
	if req.Profile != "" {
		return pickMediated(g, req, allowed, root, used)
	}
	minMiB := minMemoryMiB(req)
	var cands []Device
	for _, d := range g.Devices {
		if used.bdf[d.BDF] || !matches(d, req, minMiB) {
			continue
		}
		if root != "" && d.PCIeRoot != root {
			continue
		}
		if allowed != nil && !allowed[d.NUMA] {
			continue
		}
		cands = append(cands, d)
	}
	need := req.DeviceCount()
	if req.Aligned(model.AlignSameNUMA) {
		byNode := map[int][]Device{}
		var ids []int
		for _, d := range cands {
			if d.NUMA < 0 {
				continue
			}
			if _, ok := byNode[d.NUMA]; !ok {
				ids = append(ids, d.NUMA)
			}
			byNode[d.NUMA] = append(byNode[d.NUMA], d)
		}
		sort.Ints(ids)
		cands = nil
		for _, id := range ids {
			if len(byNode[id]) >= need {
				cands = byNode[id]
				break
			}
		}
	}
	if len(cands) < need {
		return nil, fmt.Sprintf("needs %s, %d free%s", describe(req), len(cands), localSuffix(allowed))
	}
	out := make([]model.AllocatedDevice, 0, need)
	for _, d := range cands[:need] {
		used.bdf[d.BDF] = true
		ad := model.AllocatedDevice{Name: req.Name, BDF: d.BDF, PCIeRoot: d.PCIeRoot}
		if d.NUMA >= 0 {
			n := d.NUMA
			ad.NUMANode = &n
		}
		out = append(out, ad)
	}
	return out, ""
}

type mediatedCand struct {
	parent MediatedParent
	typ    model.TopologyMediatedType
	free   int
}

// pickMediated picks req.Count instances of req.Profile, filling the
// lowest-named parent before the next one.
func pickMediated(g Graph, req model.MachineDevice, allowed map[int]bool, root string, used *usage) ([]model.AllocatedDevice, string) {
	minMiB := minMemoryMiB(req)
	var cands []mediatedCand
	for _, p := range g.Mediated {
		if p.Class != req.Class {
			continue
		}
		if req.Vendor != "" && !strings.EqualFold(p.Vendor, model.PCIVendorID(req.Vendor)) {
			continue
		}
		if req.Model != "" && !strings.EqualFold(p.Device, req.Model) {
			continue
		}
		if root != "" && p.PCIeRoot != root {
			continue
		}
		if allowed != nil && !allowed[p.NUMA] {
			continue
		}
		for _, t := range p.Types {
			if !t.Matches(req.Profile) || (minMiB > 0 && uint64(t.FramebufferMiB) < minMiB) {
				continue
			}
			if free := p.Free(t, used.mdevMiB[p.Parent], used.mdevN[p.Parent+"/"+t.ID]); free > 0 {
				cands = append(cands, mediatedCand{parent: p, typ: t, free: free})
			}
			break
		}
	}
	need := req.DeviceCount()
	if req.Aligned(model.AlignSameNUMA) {
		byNode := map[int][]mediatedCand{}
		total := map[int]int{}
		var ids []int
		for _, c := range cands {
			if c.parent.NUMA < 0 {
				continue
			}
			if _, ok := byNode[c.parent.NUMA]; !ok {
				ids = append(ids, c.parent.NUMA)
			}
			byNode[c.parent.NUMA] = append(byNode[c.parent.NUMA], c)
			total[c.parent.NUMA] += c.free
		}
		sort.Ints(ids)
		cands = nil
		for _, id := range ids {
			if total[id] >= need {
				cands = byNode[id]
				break
			}
		}
	}
	free := 0
	for _, c := range cands {
		free += c.free
	}
	if free < need {
		return nil, fmt.Sprintf("needs %s, %d free%s", describe(req), free, localSuffix(allowed))
	}
	out := make([]model.AllocatedDevice, 0, need)
	for _, c := range cands {
		for k := 0; k < c.free && len(out) < need; k++ {
			p := c.parent
			used.mdevMiB[p.Parent] += c.typ.FramebufferMiB
			used.mdevN[p.Parent+"/"+c.typ.ID]++
			ad := model.AllocatedDevice{
				Name: req.Name, PCIeRoot: p.PCIeRoot,
				Parent: p.Parent, Profile: c.typ.ID, FramebufferMiB: c.typ.FramebufferMiB,
			}
			if p.PCI() {
				ad.BDF = p.Parent
			}
			if p.NUMA >= 0 {
				n := p.NUMA
				ad.NUMANode = &n
			}
			out = append(out, ad)
		}
	}
	return out, ""
}

// VRAM is a node's GPU memory and the part Machines are allocated.
type VRAM struct {
	TotalMiB     int64
	AllocatedMiB int64
}

// VRAMUsage adds up nt's GPU memory: whole GPUs by their VRAM, vGPU parents
// by their reported VRAM or, when the host does not say, by what their
// existing instances use plus the largest free profile pool. Allocated is
// whole GPUs held by Machines on the node plus their vGPU framebuffers.
func VRAMUsage(nt model.NodeTopology, node string, machines []model.Machine) VRAM {
	whole := map[string]int64{}
	for _, d := range nt.Status.Devices {
		if d.Class == model.DeviceClassGPU {
			whole[d.BDF] = int64(d.VRAMGiB) * 1024
		}
	}
	existing := map[string]int64{}
	var v VRAM
	for _, o := range machines {
		if o.Spec.NodeName != node || o.Metadata.DeletionTimestamp != nil || o.Spec.TopologyAllocation == nil {
			continue
		}
		for _, d := range o.Spec.TopologyAllocation.Devices {
			if d.Mediated() {
				v.AllocatedMiB += d.FramebufferMiB
				existing[d.Parent+"/"+strings.ToLower(d.UUID)] = d.FramebufferMiB
				continue
			}
			v.AllocatedMiB += whole[d.BDF]
		}
	}
	for _, p := range nt.Status.MediatedDevices {
		total := p.VRAMMiB
		if total == 0 {
			total = whole[p.Parent]
		}
		if total == 0 {
			var pool int64
			for _, t := range p.Types {
				if s := int64(t.Available) * t.FramebufferMiB; s > pool {
					pool = s
				}
			}
			for _, u := range p.Instances {
				pool += existing[p.Parent+"/"+strings.ToLower(u)]
			}
			total = pool
		}
		v.TotalMiB += total
		delete(whole, p.Parent)
	}
	for _, mib := range whole {
		v.TotalMiB += mib
	}
	return v
}
