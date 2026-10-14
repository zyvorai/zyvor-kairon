// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func cmdSnapshot(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(errors.New("usage: kaironctl snapshot MACHINE [--name NAME] [--class CSI_CLASS] [--volume NAME ...]"))
	}
	machine := args[0]
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	name := fs.String("name", "", "MachineSnapshot name")
	class := fs.String("class", "", "VolumeSnapshotClass name")
	var volumes stringSliceFlag
	fs.Var(&volumes, "volume", "snapshot only this spec.volumes name (repeatable); default all")
	_ = fs.Parse(args[1:])
	out, err := createSnapshot(ctx, kc, *ns, machine, *name, *class, volumes...)
	if err != nil {
		fatal(err)
	}
	okf("machinesnapshot/%s created", out.Metadata.Name)
}

// createSnapshot creates a MachineSnapshot; an empty name is generated
// from the Machine name and the current time.
func createSnapshot(ctx context.Context, kc *kube.Client, namespace, machine, name, class string, volumes ...string) (model.MachineSnapshot, error) {
	if name == "" {
		name = resourceName(machine + "-" + time.Now().UTC().Format("20060102-150405"))
	}
	snapshot := model.MachineSnapshot{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshot},
		Metadata: model.ObjectMeta{Name: name, Namespace: namespace},
		Spec:     model.MachineSnapshotSpec{MachineName: machine, VolumeSnapshotClassName: class, VolumeNames: volumes},
	}
	return kc.CreateMachineSnapshot(ctx, namespace, snapshot)
}

func cmdRestore(ctx context.Context, kc *kube.Client, args []string) {
	if len(args) < 1 {
		fatal(fmt.Errorf("usage: kaironctl restore SNAPSHOT --target-claim NAME [--volume NAME] [flags]"))
	}
	snapshot := args[0]
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	ns := fs.String("namespace", "default", "namespace")
	name := fs.String("name", "", "MachineSnapshotRestore name")
	volume := fs.String("volume", "", "which snapshotted volume to restore; required when the snapshot covers more than one")
	targetClaim := fs.String("target-claim", "", "name for the new PersistentVolumeClaim (required)")
	storageClass := fs.String("storage-class", "", "StorageClass for the new PVC; empty uses the cluster default")
	storageSize := fs.String("storage-size", "", "size for the new PVC; empty defaults to the VolumeSnapshot's own reported restoreSize")
	machine := fs.String("machine", "", "also create a Machine with this name that boots from the restored claim (the boot volume must be the one restored)")
	start := fs.Bool("start", false, "with --machine, create it Running instead of Stopped")
	_ = fs.Parse(args[1:])
	if *targetClaim == "" {
		fatal(fmt.Errorf("--target-claim is required"))
	}
	if *name == "" {
		*name = resourceName(snapshot + "-restore-" + time.Now().UTC().Format("20060102-150405"))
	}
	restore := model.MachineSnapshotRestore{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineSnapshotRestore},
		Metadata: model.ObjectMeta{Name: *name, Namespace: *ns},
		Spec: model.MachineSnapshotRestoreSpec{
			SnapshotName:     snapshot,
			VolumeName:       *volume,
			TargetClaimName:  *targetClaim,
			StorageClassName: *storageClass,
			StorageSize:      *storageSize,
		},
	}
	if *machine != "" {
		power := "Stopped"
		if *start {
			power = "Running"
		}
		restore.Spec.Machine = &model.RestoreMachineSpec{Name: *machine, PowerState: power}
	}
	out, err := kc.CreateMachineSnapshotRestore(ctx, *ns, restore)
	if err != nil {
		fatal(err)
	}
	okf("machinesnapshotrestore/%s created", out.Metadata.Name)
	if *machine != "" {
		fmt.Printf("machine %q will be created from the snapshot, booting from %q\n", *machine, *targetClaim)
		return
	}
	fmt.Printf("once Succeeded, point a new Machine's spec.volumes[0].claimName at %q\n", *targetClaim)
}

var invalidResourceName = regexp.MustCompile(`[^a-z0-9-]+`)
