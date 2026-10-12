// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/topology"
)

func newNodeTopologyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "topology NODE",
		Short: "Show a node's NUMA nodes, devices and which Machines hold them",
		Long: `Prints the node's NodeTopology as a tree: each NUMA node with its CPUs,
memory and hugepages, the PCI devices attached to it, and the Machines whose
spec.topologyAllocation uses them. kairon-node publishes NodeTopology from
FluxVM's GET /v1/host/topology.`,
		Example: `  kaironctl node topology worker-1`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			nt, err := kc.GetNodeTopology(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("NodeTopology %s: %w", args[0], err)
			}
			machines, err := kc.ListMachines(cmd.Context())
			if err != nil {
				return err
			}
			printNodeTopology(os.Stdout, nt, machines)
			return nil
		},
	}
}

func printNodeTopology(w io.Writer, nt model.NodeTopology, machines []model.Machine) {
	cpuOwner := map[int][]string{}
	devOwner := map[string]string{}
	for _, m := range machines {
		a := m.Spec.TopologyAllocation
		if m.Spec.NodeName != nt.Metadata.Name || a == nil {
			continue
		}
		ref := m.Namespace() + "/" + m.Metadata.Name
		for _, n := range a.NUMANodes {
			cpuOwner[n.ID] = append(cpuOwner[n.ID], fmt.Sprintf("%s cpus=%s mem=%dMiB", ref, n.CPUs, n.MemoryMiB))
		}
		for _, d := range a.Devices {
			if d.Mediated() {
				devOwner[strings.ToLower(d.UUID)] = ref
				continue
			}
			devOwner[d.BDF] = ref
		}
	}
	binding := "no"
	if nt.Status.NUMABinding {
		binding = "yes"
	}
	_, _ = fmt.Fprintf(w, "%s (numa binding: %s)\n", nt.Metadata.Name, binding)
	byNUMA := map[int][]model.TopologyDevice{}
	var remote []model.TopologyDevice
	for _, d := range nt.Status.Devices {
		if d.NUMANode == nil {
			remote = append(remote, d)
			continue
		}
		byNUMA[*d.NUMANode] = append(byNUMA[*d.NUMANode], d)
	}
	for i, n := range nt.Status.NUMANodes {
		last := i == len(nt.Status.NUMANodes)-1 && len(remote) == 0
		branch, pad := treeBranch(last)
		_, _ = fmt.Fprintf(w, "%snuma%d  cpus %s  mem %dMiB (%dMiB free)%s\n", branch, n.ID, n.CPUs, n.MemoryMiB, n.FreeMemoryMiB, hugepageSummary(n.Hugepages))
		var lines []string
		for _, d := range byNUMA[n.ID] {
			lines = append(lines, deviceLine(d, devOwner[d.BDF]))
		}
		for _, o := range cpuOwner[n.ID] {
			lines = append(lines, "machine "+o)
		}
		for j, l := range lines {
			b, _ := treeBranch(j == len(lines)-1)
			_, _ = fmt.Fprintf(w, "%s%s%s\n", pad, b, l)
		}
	}
	if len(remote) > 0 {
		_, _ = fmt.Fprintf(w, "└── no NUMA node\n")
		for j, d := range remote {
			b, _ := treeBranch(j == len(remote)-1)
			_, _ = fmt.Fprintf(w, "    %s%s\n", b, deviceLine(d, devOwner[d.BDF]))
		}
	}
	printMediated(w, nt, devOwner)
	if v := topology.VRAMUsage(nt, nt.Metadata.Name, machines); v.TotalMiB > 0 || v.AllocatedMiB > 0 {
		_, _ = fmt.Fprintf(w, "GPU memory: %dMiB allocated of %dMiB\n", v.AllocatedMiB, v.TotalMiB)
	}
}

func printMediated(w io.Writer, nt model.NodeTopology, owner map[string]string) {
	if len(nt.Status.MediatedDevices) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "vGPU parents")
	for i, p := range nt.Status.MediatedDevices {
		branch, pad := treeBranch(i == len(nt.Status.MediatedDevices)-1)
		head := p.Parent
		if p.Vendor != "" {
			head += fmt.Sprintf(" %s:%s", p.Vendor, p.Device)
		} else {
			head += " (virtual)"
		}
		if p.VRAMMiB > 0 {
			head += fmt.Sprintf(" %dMiB", p.VRAMMiB)
		}
		head += healthSuffix(p.Health, p.HealthReasons)
		if !p.Allocatable {
			head += " [not in --mdev-parents]"
		}
		_, _ = fmt.Fprintf(w, "%s%s\n", branch, head)
		var lines []string
		for _, t := range p.Types {
			l := t.ID
			if t.Name != "" && t.Name != t.ID {
				l += fmt.Sprintf(" %q", t.Name)
			}
			if t.FramebufferMiB > 0 {
				l += fmt.Sprintf(" %dMiB", t.FramebufferMiB)
			}
			if t.MIGSlices > 0 {
				l += fmt.Sprintf(" MIG %d slices/%dGiB", t.MIGSlices, t.MIGMemoryGiB)
			}
			lines = append(lines, l+fmt.Sprintf(" available %d", t.Available))
		}
		for _, u := range p.Instances {
			l := "instance " + u
			if o := owner[strings.ToLower(u)]; o != "" {
				l += " -> " + o
			}
			lines = append(lines, l)
		}
		for j, l := range lines {
			b, _ := treeBranch(j == len(lines)-1)
			_, _ = fmt.Fprintf(w, "%s%s%s\n", pad, b, l)
		}
	}
}

func healthSuffix(h string, reasons []string) string {
	if h == "" || h == model.DeviceHealthy {
		return ""
	}
	if len(reasons) == 0 {
		return " [" + h + "]"
	}
	return " [" + h + ": " + strings.Join(reasons, "; ") + "]"
}

func treeBranch(last bool) (string, string) {
	if last {
		return "└── ", "    "
	}
	return "├── ", "│   "
}

func hugepageSummary(hp []model.TopologyHugepages) string {
	if len(hp) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hp))
	for _, h := range hp {
		parts = append(parts, fmt.Sprintf("%s %d/%d free", h.Size, h.Free, h.Total))
	}
	sort.Strings(parts)
	return "  hugepages " + strings.Join(parts, ", ")
}

func deviceLine(d model.TopologyDevice, owner string) string {
	s := fmt.Sprintf("%s %s %s:%s root %s", d.Class, d.BDF, d.Vendor, d.Device, d.PCIeRoot)
	if d.PhysFn != "" {
		s += " vf-of " + d.PhysFn
	}
	if d.TotalVFs > 0 {
		s += fmt.Sprintf(" vfs %d", d.TotalVFs)
	}
	if d.VRAMGiB > 0 {
		s += fmt.Sprintf(" %dGiB", d.VRAMGiB)
	}
	s += healthSuffix(d.Health, d.HealthReasons)
	switch {
	case owner != "":
		s += " -> " + owner
	case d.Allocated:
		s += " [in use outside Kairon]"
	case !d.Allocatable:
		s += " [not in VFIO allowlist]"
	}
	return s
}
