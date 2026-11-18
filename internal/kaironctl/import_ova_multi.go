// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
	sigsyaml "sigs.k8s.io/yaml"
)

// parseOVFPackage reads an OVF with one or more VirtualSystem entries
// (directly under Envelope or inside any VirtualSystemCollection) and returns
// one ovfInfo per VirtualSystem in document order. The second result lists
// everything in the descriptor that kaironctl does not convert, one
// human-readable sentence each (without the "warning:" prefix). A single-VM
// OVF returns exactly one ovfInfo with the DiskSection count, as parseOVF does.
func parseOVFPackage(data []byte) ([]ovfInfo, []string, error) {
	var env ovfEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return nil, nil, fmt.Errorf("parse OVF: %w", err)
	}
	if len(env.Collections) == 0 && len(env.Systems) <= 1 {
		info, err := parseOVF(data)
		if err != nil {
			return nil, nil, err
		}
		return []ovfInfo{info}, nil, nil
	}

	var systems []ovfSystem
	var warns []string
	systems = append(systems, env.Systems...)
	var walk func(c ovfCollection)
	walk = func(c ovfCollection) {
		label := firstNonEmpty(c.Name, c.ID, "unnamed")
		if len(c.Startup) > 0 {
			ids := make([]string, 0, len(c.Startup))
			for _, it := range c.Startup {
				ids = append(ids, it.ID+"(order="+it.Order+")")
			}
			warns = append(warns, fmt.Sprintf("collection %q: startup ordering %s not converted; the Machines start independently", label, strings.Join(ids, ", ")))
		}
		if len(c.Props) > 0 {
			warns = append(warns, fmt.Sprintf("collection %q: vApp properties %s not converted", label, propKeys(c.Props)))
		}
		systems = append(systems, c.Systems...)
		for _, ch := range c.Children {
			walk(ch)
		}
	}
	for _, c := range env.Collections {
		walk(c)
	}
	if len(systems) == 0 {
		return nil, nil, fmt.Errorf("OVF has a VirtualSystemCollection but no VirtualSystem")
	}

	infos := make([]ovfInfo, 0, len(systems))
	diskUsers := map[string][]string{} // HostResource -> VM names, in order
	var diskOrder []string
	for i, sys := range systems {
		info := systemInfo(sys)
		if info.Name == "" {
			info.Name = "vm-" + strconv.Itoa(i+1)
		}
		seen := map[string]bool{}
		for _, it := range sys.Items {
			if strings.TrimSpace(it.ResourceType) != "17" {
				continue
			}
			info.Disks++
			ref := strings.TrimSpace(it.HostResource)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			if _, ok := diskUsers[ref]; !ok {
				diskOrder = append(diskOrder, ref)
			}
			diskUsers[ref] = append(diskUsers[ref], info.Name)
		}
		if len(sys.Props) > 0 {
			warns = append(warns, fmt.Sprintf("VM %q: vApp properties %s not converted", info.Name, propKeys(sys.Props)))
		}
		if info.Disks > 1 {
			warns = append(warns, fmt.Sprintf("VM %q: has %d disks; only the boot disk is attached", info.Name, info.Disks))
		}
		if info.Firmware == "efi" {
			warns = append(warns, fmt.Sprintf("VM %q: boots UEFI; schedule it on a node whose FluxVM sets qemu_ovmf_code", info.Name))
		}
		infos = append(infos, info)
	}
	for _, ref := range diskOrder {
		if users := diskUsers[ref]; len(users) > 1 {
			warns = append(warns, fmt.Sprintf("disk %s is shared by VMs %s; shared disks are not converted, each Machine would need its own volume", ref, strings.Join(users, ", ")))
		}
	}
	warns = append(warns, fmt.Sprintf("all %d Machines reference the same OVA; kairon-node hands the whole OVA to FluxVM and does not pick a per-VM disk, so check which disk each Machine boots", len(infos)))
	return infos, warns, nil
}

func propKeys(ps []ovfProp) string {
	keys := make([]string, 0, len(ps))
	for _, p := range ps {
		keys = append(keys, p.Key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// uniqueMachineNames makes each VM name DNS-safe (resourceName) and unique:
// a repeat gets -2, -3, ... (the suffix is kept inside 63 characters). A name
// that sanitises to nothing becomes vm-N. The second result describes renames.
func uniqueMachineNames(vmNames []string) ([]string, []string) {
	used := map[string]bool{}
	out := make([]string, len(vmNames))
	var notes []string
	for i, vm := range vmNames {
		base := resourceName(vm)
		if base == "" {
			base = "vm-" + strconv.Itoa(i+1)
		}
		name := base
		for n := 2; used[name]; n++ {
			suffix := "-" + strconv.Itoa(n)
			b := base
			if len(b)+len(suffix) > 63 {
				b = strings.TrimRight(b[:63-len(suffix)], "-")
			}
			name = b + suffix
		}
		used[name] = true
		out[i] = name
		if name != vm {
			notes = append(notes, fmt.Sprintf("VM %q becomes Machine %q", vm, name))
		}
	}
	return out, notes
}

// multiImportOpts carries what cmdImport has already resolved.
type multiImportOpts struct {
	source, url, namespace, name string
	sum                          string
	size                         int64
	noRepair                     bool
	set                          map[string]bool
	specFn                       func() model.MachineSpec
}

// buildImportMachines turns the VMs of a multi-VM OVF into Machines, mapping
// CPU and memory the way the single-VM path does (flags win over the OVF).
func buildImportMachines(infos []ovfInfo, o multiImportOpts) ([]model.Machine, []string) {
	vmNames := make([]string, len(infos))
	for i, in := range infos {
		vmNames[i] = in.Name
	}
	names, notes := uniqueMachineNames(vmNames)
	ms := make([]model.Machine, 0, len(infos))
	for i, info := range infos {
		spec := o.specFn()
		if !o.set["cpu"] && info.VCPUs > 0 {
			spec.Resources.CPU = strconv.Itoa(info.VCPUs)
		}
		if !o.set["memory"] && info.MemoryMiB > 0 {
			spec.Resources.Memory = strconv.FormatInt(info.MemoryMiB, 10) + "Mi"
		}
		spec.Image = model.ImageSpec{
			Digest: o.sum,
			Source: &model.ImageSource{HTTPURL: o.url, Format: "ova", Repair: !o.noRepair},
		}
		ms = append(ms, model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{Name: names[i], Namespace: o.namespace, Labels: map[string]string{"kairon.zyvor.dev/imported-from": "ova"}},
			Spec:     spec,
		})
	}
	return ms, notes
}

// writeMachinesYAML prints Machines as a multi-document YAML stream.
func writeMachinesYAML(w io.Writer, ms []model.Machine) error {
	for i, m := range ms {
		b, err := sigsyaml.Marshal(m)
		if err != nil {
			return err
		}
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return err
			}
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// importOVAMulti handles an OVA that holds several VMs: it prints one Machine
// per VM as a multi-document YAML stream on stdout (it never creates anything;
// review it and pipe it to `kaironctl apply -f -`) and reports what was not
// converted as `warning:` lines on stderr.
func importOVAMulti(_ context.Context, infos []ovfInfo, warns []string, o multiImportOpts) {
	if o.name != "" {
		fatal(fmt.Errorf("--name cannot be used with a multi-VM OVA (%d VMs); Machine names come from the VM names", len(infos)))
	}
	_, _ = fmt.Fprintf(os.Stderr, "read %s: %d bytes, %s, %d VMs\n", o.source, o.size, o.sum, len(infos))
	ms, notes := buildImportMachines(infos, o)
	for _, n := range notes {
		_, _ = fmt.Fprintf(os.Stderr, "note: %s\n", n)
	}
	for _, w := range warns {
		_, _ = fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	if err := writeMachinesYAML(os.Stdout, ms); err != nil {
		fatal(err)
	}
}
