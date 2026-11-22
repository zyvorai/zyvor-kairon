// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// withDisk returns spec.disks with name attached to claim (add=true) or
// removed (add=false).
func withDisk(disks []model.MachineDisk, name, claim string, add bool) ([]model.MachineDisk, error) {
	i := slices.IndexFunc(disks, func(d model.MachineDisk) bool { return d.Name == name })
	out := slices.Clone(disks)
	switch {
	case add && i >= 0:
		return nil, fmt.Errorf("disk %q is already in spec.disks", name)
	case add:
		out = append(out, model.MachineDisk{Name: name, ClaimName: claim})
		return out, model.ValidateDisks(out)
	case i < 0:
		return nil, fmt.Errorf("disk %q is not in spec.disks", name)
	default:
		return slices.Delete(out, i, i+1), nil
	}
}

// withInterface is withDisk for spec.network.extraInterfaces.
func withInterface(ns model.NetworkSpec, iface model.ExtraInterface, add bool) ([]model.ExtraInterface, error) {
	list := ns.ExtraInterfaces
	i := slices.IndexFunc(list, func(x model.ExtraInterface) bool { return x.Name == iface.Name })
	out := slices.Clone(list)
	switch {
	case add && i >= 0:
		return nil, fmt.Errorf("interface %q is already in spec.network.extraInterfaces", iface.Name)
	case add:
		out = append(out, iface)
		ns.ExtraInterfaces = out
		return out, model.ValidateExtraInterfaces(ns)
	case i < 0:
		return nil, fmt.Errorf("interface %q is not in spec.network.extraInterfaces", iface.Name)
	default:
		return slices.Delete(out, i, i+1), nil
	}
}

func cmdDisk(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 2 {
		fatal(fmt.Errorf("usage: kaironctl disk attach|detach|list MACHINE [NAME] [--claim PVC]"))
	}
	verb, machine := args[0], args[1]
	fs := flag.NewFlagSet("disk", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	claim := fs.String("claim", "", "PersistentVolumeClaim to attach (attach only)")
	rest := args[2:]
	name := ""
	if verb != "list" {
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: kaironctl disk %s MACHINE NAME", verb))
		}
		name, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	m, err := kc.GetMachine(ctx, *ns, machine)
	if err != nil {
		fatal(err)
	}
	var disks []model.MachineDisk
	switch verb {
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tCLAIM\tATTACHED")
		for _, d := range m.Spec.Disks {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%v\n", d.Name, d.ClaimName, slices.Contains(m.Status.AttachedDisks, d.Name))
		}
		_ = w.Flush()
		return
	case "attach":
		if *claim == "" {
			fatal(fmt.Errorf("--claim is required"))
		}
		disks, err = withDisk(m.Spec.Disks, name, *claim, true)
	case "detach":
		disks, err = withDisk(m.Spec.Disks, name, "", false)
	default:
		fatal(fmt.Errorf("unknown disk verb %q (attach, detach, list)", verb))
	}
	if err != nil {
		fatal(err)
	}
	if err := kc.PatchMachine(ctx, *ns, machine, map[string]any{
		"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
		"spec":     map[string]any{"disks": disks},
	}); err != nil {
		fatal(err)
	}
	okf("machine/%s: disk %s %s (kairon-node applies it live)", machine, name, map[string]string{"attach": "attached", "detach": "detached"}[verb])
}

func cmdNIC(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 2 {
		fatal(fmt.Errorf("usage: kaironctl nic add|remove|list MACHINE [NAME] [--bridge BR] [--mac MAC]"))
	}
	verb, machine := args[0], args[1]
	fs := flag.NewFlagSet("nic", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	bridge := fs.String("bridge", "", "host bridge (add only)")
	mac := fs.String("mac", "", "MAC address (add only; default derived from the Machine)")
	rest := args[2:]
	name := ""
	if verb != "list" {
		if len(rest) < 1 {
			fatal(fmt.Errorf("usage: kaironctl nic %s MACHINE NAME", verb))
		}
		name, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	m, err := kc.GetMachine(ctx, *ns, machine)
	if err != nil {
		fatal(err)
	}
	var list []model.ExtraInterface
	switch verb {
	case "list":
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tBRIDGE\tMAC\tATTACHED")
		for _, iface := range m.Spec.Network.ExtraInterfaces {
			mac := model.ExtraInterfaceMAC(m.Metadata.UID, iface)
			attached := slices.ContainsFunc(m.Status.AttachedInterfaces, func(a model.AttachedInterface) bool { return a.MAC == mac })
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%v\n", iface.Name, iface.Bridge, mac, attached)
		}
		_ = w.Flush()
		return
	case "add":
		if *bridge == "" {
			fatal(fmt.Errorf("--bridge is required"))
		}
		list, err = withInterface(m.Spec.Network, model.ExtraInterface{Name: name, Bridge: *bridge, MAC: *mac}, true)
	case "remove":
		list, err = withInterface(m.Spec.Network, model.ExtraInterface{Name: name}, false)
	default:
		fatal(fmt.Errorf("unknown nic verb %q (add, remove, list)", verb))
	}
	if err != nil {
		fatal(err)
	}
	if err := kc.PatchMachine(ctx, *ns, machine, map[string]any{
		"metadata": map[string]any{"resourceVersion": m.Metadata.ResourceVersion},
		"spec":     map[string]any{"network": map[string]any{"extraInterfaces": list}},
	}); err != nil {
		fatal(err)
	}
	past := map[string]string{"add": "added", "remove": "removed"}[verb]
	okf("machine/%s: interface %s %s (kairon-node applies it live)", machine, name, past)
}
