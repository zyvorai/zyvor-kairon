// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"flag"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// cloneSystemLabels describe the source's place in a set, pool or fork tree
// and never carry over to a clone.
var cloneSystemLabels = map[string]bool{
	model.LabelMachinePool:            true,
	model.LabelMachinePoolTemplate:    true,
	model.LabelPoolState:              true,
	model.LabelMachineClaim:           true,
	model.LabelMachineSet:             true,
	model.LabelMachineSetTemplateHash: true,
	model.LabelForkedFrom:             true,
	model.LabelClonedFrom:             true,
	model.AssignedNodeLabel:           true,
}

// filterKeys keeps the keys matching the glob filters, virtctl-style: a
// key is kept when it matches a pattern and no later "!pattern".
func filterKeys(in map[string]string, filters []string, drop func(string) bool) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if drop(k) {
			continue
		}
		keep := false
		for _, f := range filters {
			neg := strings.HasPrefix(f, "!")
			if ok, _ := path.Match(strings.TrimPrefix(f, "!"), k); ok {
				keep = !neg
			}
		}
		if keep {
			out[k] = v
		}
	}
	return out
}

// cloneMachine builds the new Machine. A Running source is forked (memory
// and disk, copy-on-write); a Halted one gets a flattened copy of its disk
// made by kairon-node before first boot. Both land on the source's node.
func cloneMachine(src model.Machine, name string, labelFilters, annotationFilters []string) (model.Machine, error) {
	ref := src.Namespace() + "/" + src.Metadata.Name
	if len(src.Spec.Volumes) > 0 || len(src.Spec.Disks) > 0 || len(src.Spec.DeviceClaims) > 0 {
		return model.Machine{}, fmt.Errorf("machine %s has volumes, disks or device claims; clone copies only the boot disk -- snapshot and restore the PVCs instead (kaironctl snapshot / restore)", ref)
	}
	if src.Spec.NodeName == "" {
		return model.Machine{}, fmt.Errorf("machine %s is not scheduled to a node", ref)
	}
	if len(labelFilters) == 0 {
		labelFilters = []string{"*"}
	}
	labels := filterKeys(src.Metadata.Labels, labelFilters, func(k string) bool { return cloneSystemLabels[k] })
	annotations := filterKeys(src.Metadata.Annotations, annotationFilters, func(k string) bool {
		return strings.HasPrefix(k, "kairon.zyvor.dev/") || k == "kubectl.kubernetes.io/last-applied-configuration"
	})
	labels[model.AssignedNodeLabel] = src.Spec.NodeName
	spec := src.Spec
	spec.PowerState = "Running"
	spec.Network.MAC = ""
	spec.Network.TapName = ""
	if spec.CloudInit.Hostname != "" {
		spec.CloudInit.Hostname = name
	}
	switch src.Status.Phase {
	case "Running":
		labels[model.LabelForkedFrom] = src.Metadata.Name
		annotations[model.AnnotationForkFrom] = src.Metadata.Name
	case "Halted":
		labels[model.LabelClonedFrom] = src.Metadata.Name
		annotations[model.AnnotationCloneFrom] = src.Metadata.Name
	default:
		return model.Machine{}, fmt.Errorf("machine %s is %q; clone needs Running (forked live) or Halted (disk copied) -- a Stopped Machine without volumes has no disk left", ref, src.Status.Phase)
	}
	return model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: name, Namespace: src.Namespace(), Labels: labels, Annotations: annotations},
		Spec:     spec,
	}, nil
}

func cmdCreateClone(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fatal(fmt.Errorf("usage: kaironctl create clone SOURCE --name NEW [--label-filter GLOB]... [--annotation-filter GLOB]... [--wait 5m | --no-wait]"))
	}
	source := args[0]
	fs := flag.NewFlagSet("create clone", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	name := fs.String("name", "", "name of the new Machine (default SOURCE-clone-TIMESTAMP)")
	var labelFilters, annotationFilters stringSliceFlag
	fs.Var(&labelFilters, "label-filter", "copy source labels matching this glob; prefix ! to exclude (repeatable, default *)")
	fs.Var(&annotationFilters, "annotation-filter", "copy source annotations matching this glob; prefix ! to exclude (repeatable, default none)")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for the clone to run")
	noWait := fs.Bool("no-wait", false, "create the clone and return immediately")
	co := createOutputFlags(fs)
	_ = fs.Parse(args[1:])
	if kc == nil {
		fatal(fmt.Errorf("create clone reads the source Machine, so --dry-run still needs cluster access"))
	}
	if *name == "" {
		*name = source + "-clone-" + time.Now().UTC().Format("20060102150405")
	}
	src, err := kc.GetMachine(ctx, *ns, source)
	if err != nil {
		fatal(err)
	}
	m, err := cloneMachine(src, *name, labelFilters, annotationFilters)
	if err != nil {
		fatal(err)
	}
	if co.preview(m) {
		return
	}
	out, err := kc.CreateMachine(ctx, *ns, m)
	if err != nil {
		fatal(err)
	}
	how := "forked from the running source"
	if src.Status.Phase == "Halted" {
		how = "kairon-node is copying the disk"
	}
	if *noWait || co.format != "" {
		co.done(out, "machine/%s created (%s)", out.Metadata.Name, how)
		return
	}
	if err := waitForRunning(context.Background(), kc, *ns, []string{*name}, *wait); err != nil {
		fatal(err)
	}
	okf("machine/%s cloned from %s (%s)", *name, source, how)
}
