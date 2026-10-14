// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/zyvorai/kairon/internal/model"
)

// permittedDevice is one PCI device or vGPU profile a Machine may ask for.
type permittedDevice struct {
	Node   string `json:"node"`
	Kind   string `json:"kind"` // pci|sriov-vf|vgpu
	Class  string `json:"class"`
	ID     string `json:"id"`    // BDF, or mdev type for vgpu
	Model  string `json:"model"` // vendor:device or profile name
	Parent string `json:"parent,omitempty"`
	NUMA   string `json:"numa,omitempty"`
	Health string `json:"health,omitempty"`
	// Free is 1/0 for a PCI device; for a vGPU profile, the instances the
	// parent can still create.
	Free int    `json:"free"`
	Note string `json:"note,omitempty"`
}

func permittedDevices(nts []model.NodeTopology) []permittedDevice {
	var out []permittedDevice
	for _, nt := range nts {
		node := nt.Metadata.Name
		for _, d := range nt.Status.Devices {
			if !d.Allocatable {
				continue
			}
			p := permittedDevice{Node: node, Kind: "pci", Class: d.Class, ID: d.BDF, Model: d.Vendor + ":" + d.Device, Health: d.Health}
			if d.PhysFn != "" {
				p.Kind, p.Parent = "sriov-vf", d.PhysFn
			}
			if d.NUMANode != nil {
				p.NUMA = strconv.Itoa(*d.NUMANode)
			}
			if !d.Allocated && d.Health != model.DeviceUnhealthy {
				p.Free = 1
			}
			var notes []string
			if d.VRAMGiB > 0 {
				notes = append(notes, fmt.Sprintf("%dGiB", d.VRAMGiB))
			}
			if d.TotalVFs > 0 {
				notes = append(notes, fmt.Sprintf("%d VFs", d.TotalVFs))
			}
			if d.Allocated {
				notes = append(notes, "in use")
			}
			p.Note = strings.Join(append(notes, d.HealthReasons...), "; ")
			out = append(out, p)
		}
		for _, parent := range nt.Status.MediatedDevices {
			if !parent.Allocatable {
				continue
			}
			numa := ""
			if parent.NUMANode != nil {
				numa = strconv.Itoa(*parent.NUMANode)
			}
			for _, t := range parent.Types {
				name := t.Name
				if name == "" {
					name = t.ID
				}
				free := t.Available
				if parent.Health == model.DeviceUnhealthy {
					free = 0
				}
				var notes []string
				if t.FramebufferMiB > 0 {
					notes = append(notes, fmt.Sprintf("%dMiB", t.FramebufferMiB))
				}
				if t.MIGSlices > 0 {
					notes = append(notes, fmt.Sprintf("MIG %d slices/%dGiB", t.MIGSlices, t.MIGMemoryGiB))
				}
				out = append(out, permittedDevice{
					Node: node, Kind: "vgpu", Class: parent.Class, ID: t.ID, Model: name,
					Parent: parent.Parent, NUMA: numa, Health: parent.Health, Free: free,
					Note: strings.Join(append(notes, parent.HealthReasons...), "; "),
				})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.ID < b.ID
	})
	return out
}

func writePermittedDevices(w io.Writer, devs []permittedDevice, format string) error {
	switch format {
	case "", "wide", "table":
		if len(devs) == 0 {
			_, _ = fmt.Fprintln(w, "No permitted devices (kairon-node --vfio-allowlist / --mdev-parents are empty or no NodeTopology yet).")
			return nil
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NODE\tKIND\tCLASS\tID\tMODEL\tPARENT\tNUMA\tHEALTH\tFREE\tNOTE")
		for _, d := range devs {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				d.Node, d.Kind, d.Class, d.ID, d.Model, dash(d.Parent), dash(d.NUMA), dash(d.Health), d.Free, d.Note)
		}
		return tw.Flush()
	case "json":
		b, err := json.MarshalIndent(devs, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, string(b))
	case "yaml":
		b, err := yaml.Marshal(devs)
		if err != nil {
			return err
		}
		_, _ = w.Write(b)
	default:
		return fmt.Errorf("unknown --output %q (table|json|yaml)", format)
	}
	return nil
}

func newPermittedDevicesCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "permitted-devices [NODE]",
		Short: "List the host devices and vGPU profiles Machines may request",
		Long: `Lists PCI devices in kairon-node's --vfio-allowlist (GPUs, NICs, NVMe,
SR-IOV virtual functions) and vGPU profiles on its --mdev-parents, with
health and how many are free, read from each node's NodeTopology -- like
virtctl adm permitted-devices.`,
		Example: `  $ kaironctl permitted-devices
  $ kaironctl permitted-devices worker-1 -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := writePermittedDevices(io.Discard, nil, output); err != nil {
				return err
			}
			kc, err := newKubeClient()
			if err != nil {
				return err
			}
			var nts []model.NodeTopology
			if len(args) == 1 {
				nt, err := kc.GetNodeTopology(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("NodeTopology %s: %w", args[0], err)
				}
				nts = []model.NodeTopology{nt}
			} else if nts, err = kc.ListNodeTopologies(cmd.Context()); err != nil {
				return err
			}
			return writePermittedDevices(cmd.OutOrStdout(), permittedDevices(nts), output)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json|yaml")
	return cmd
}
