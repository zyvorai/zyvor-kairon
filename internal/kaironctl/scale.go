// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
)

func cmdScale(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl scale machineset NAME --replicas N | kaironctl scale machineset --selector k=v --replicas N [--dry-run]"))
	}
	kind := strings.ToLower(args[0])
	if kind == "machinepool" || kind == "machinepools" {
		cmdScaleMachinePool(ctx, kc, args[1:])
		return
	}
	if kind != "machineset" && kind != "machinesets" {
		fatal(fmt.Errorf("scale supports machineset and machinepool, got %q", kind))
	}
	rest := args[1:]
	if hasFlag(rest, "selector") {
		cmdScaleSelector(ctx, kc, rest)
		return
	}
	if len(rest) < 1 {
		fatal(fmt.Errorf("usage: kaironctl scale machineset NAME --replicas N"))
	}
	name := rest[0]
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	replicas := fs.Int("replicas", -1, "desired replica count (required)")
	_ = fs.Parse(rest[1:])
	if *replicas < 0 {
		fatal(fmt.Errorf("--replicas N is required"))
	}
	if err := kc.PatchMachineSet(ctx, *ns, name, map[string]any{"spec": map[string]any{"replicas": *replicas}}); err != nil {
		fatal(err)
	}
	okf("machineset/%s scaled to %d replicas", name, *replicas)
}

// cmdScaleMachinePool sets a MachinePool's warm size.
func cmdScaleMachinePool(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl scale machinepool NAME --replicas N"))
	}
	name := args[0]
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	replicas := fs.Int("replicas", -1, "warm Machines to keep (required)")
	_ = fs.Parse(args[1:])
	if *replicas < 0 {
		fatal(fmt.Errorf("--replicas N is required"))
	}
	if err := kc.PatchMachinePool(ctx, *ns, name, map[string]any{"spec": map[string]any{"replicas": *replicas}}); err != nil {
		fatal(err)
	}
	okf("machinepool/%s scaled to %d warm replicas", name, *replicas)
}

// cmdScaleSelector implements `scale machineset --selector k=v [--selector
// k2=v2] --replicas N [--namespace NS] [--dry-run]`: patches spec.replicas
// to the SAME value on every MachineSet in ns whose labels satisfy every
// given key=value pair -- e.g. scaling every MachineSet labeled
// env=staging to 0 before a maintenance window, or every tier=web
// MachineSet up together after a capacity change. This mirrors
// cmdDeleteSelector's shape deliberately (matchingNames to resolve the
// selector, required non-empty --selector, sorted deterministic order,
// --dry-run, one PATCH per match through the exact same single-object
// kc.PatchMachineSet call the non-bulk path above uses) since it's the
// same "resolve a selector to a list of names, then act on each one
// through the existing single-object path" bulk pattern.
//
// scale is a genuinely good fit for this, unlike edit: it has exactly one
// mutable field (replicas), and a bulk caller wants that ONE value applied
// identically to every match. edit's kinds (migrationpolicy,
// snapshotschedule, quota, budget, machine) each expose several
// independent fields, and "apply the same --max-concurrent to N
// differently-configured policies at once" is a far less obviously safe
// or wanted operation than "scale everything matching this selector to
// N" -- so --selector is intentionally NOT added to cmdEdit.
func cmdScaleSelector(ctx context.Context, kc *kube.Client, args []string) {
	fs := flag.NewFlagSet("scale", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	var selectorFlag stringSliceFlag
	fs.Var(&selectorFlag, "selector", "label key=value every matching MachineSet must carry (repeatable -- every pair must match; required and must be non-empty)")
	replicas := fs.Int("replicas", -1, "desired replica count applied to every matching MachineSet (required)")
	dryRun := fs.Bool("dry-run", false, "print what would be scaled without scaling anything")
	_ = fs.Parse(args)
	if *replicas < 0 {
		fatal(fmt.Errorf("--replicas N is required"))
	}
	sel, err := parseKeyValues(selectorFlag)
	if err != nil {
		fatal(err)
	}
	if len(sel) == 0 {
		fatal(fmt.Errorf("--selector is required and must be non-empty for bulk scale (usage: kaironctl scale machineset --selector k=v --replicas N) -- an empty selector matches nothing, by design, rather than risk being misread as \"everything\""))
	}
	names, err := matchingNames(ctx, kc, *ns, "machineset", sel)
	if err != nil {
		fatal(err)
	}
	if len(names) == 0 {
		fmt.Println("no machinesets matched selector; nothing to scale")
		return
	}
	sort.Strings(names) // deterministic order for a repeatable dry-run/real-run diff
	for _, name := range names {
		if *dryRun {
			okf("machineset/%s (dry-run, not scaled)", name)
			continue
		}
		if err := kc.PatchMachineSet(ctx, *ns, name, map[string]any{"spec": map[string]any{"replicas": *replicas}}); err != nil {
			fatal(fmt.Errorf("scaling machineset/%s: %w", name, err))
		}
		okf("machineset/%s scaled to %d replicas", name, *replicas)
	}
}

// cmdEdit patches a subset of an existing object's spec fields, touching
// only the ones an explicit flag was actually passed for on this
// invocation (tracked via fs.Visit, never a flag's zero-value default) so
// an omitted flag can never clobber an already-set value back to zero.
// migrationpolicy, snapshotschedule, quota, budget, machine, networkpolicy,
