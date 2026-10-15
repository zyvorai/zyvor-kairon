// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package rehearsal

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func (r *Runner) readSource(ctx context.Context, p Plan, m Member) (model.Machine, map[string]string, error) {
	if m.SourceSnapshot == "" {
		src, err := r.Kube.GetMachine(ctx, p.Namespace, m.Name)
		return src, m.Snapshots, err
	}
	snap, err := r.Kube.GetMachineSnapshot(ctx, p.Namespace, m.SourceSnapshot)
	if err != nil {
		return model.Machine{}, nil, err
	}
	if !snap.Status.ReadyToUse || snap.Status.Phase != "Succeeded" || snap.Status.SourceSpec == nil || snap.Spec.MachineName != m.Name {
		return model.Machine{}, nil, fmt.Errorf("sourceSnapshot %s is not a ready recovery point with a recorded source spec for %s", m.SourceSnapshot, m.Name)
	}
	if p.Purpose == "incident" && m.Name == p.Incident.Machine && snap.Status.SourceUID != p.Incident.SourceUID {
		return model.Machine{}, nil, fmt.Errorf("incident UID does not match the recovery point's source UID")
	}
	points := map[string]string{}
	for _, v := range snap.Status.VolumeSnapshots {
		if !v.ReadyToUse || v.VolumeSnapshotName == "" || v.AtlasSnapshotID != "" || points[v.VolumeName] != "" {
			return model.Machine{}, nil, fmt.Errorf("sourceSnapshot needs unique ready CSI recovery points")
		}
		points[v.VolumeName] = v.VolumeSnapshotName
	}
	src := model.Machine{Metadata: model.ObjectMeta{Name: m.Name, Namespace: p.Namespace, UID: snap.Status.SourceUID, ResourceVersion: "snapshot/" + snap.Metadata.UID}, Spec: *snap.Status.SourceSpec, Status: model.MachineStatus{Phase: "Halted"}}
	return src, points, nil
}

func (r *Runner) sourceClaim(ctx context.Context, p Plan, m Member, v model.MachineVolume, point string) (model.PersistentVolumeClaim, error) {
	pvc, err := r.Kube.GetPersistentVolumeClaim(ctx, p.Namespace, v.ClaimName)
	if err == nil {
		return pvc, nil
	}
	if !kube.IsNotFound(err) || m.SourceSnapshot == "" {
		return pvc, err
	}
	// The original Machine/PVC may be gone during a recovery. Use CSI's
	// recorded source mode, and an explicitly selected class with the same driver.
	class := m.RestoreStorageClass[v.Name]
	if class == "" {
		return pvc, fmt.Errorf("missing source claim %s: restoreStorageClass is required", v.ClaimName)
	}
	snap, err := r.Kube.GetVolumeSnapshot(ctx, p.Namespace, point)
	if err != nil {
		return pvc, err
	}
	if snap.Status.BoundVolumeSnapshotContentName == "" || snap.Spec.VolumeSnapshotClassName == nil {
		return pvc, fmt.Errorf("offline recovery needs bound CSI snapshot content and class metadata")
	}
	mode, err := r.Kube.SnapshotContentMode(ctx, snap.Status.BoundVolumeSnapshotContentName)
	if err != nil {
		return pvc, err
	}
	if mode != "Filesystem" {
		return pvc, fmt.Errorf("offline recovery requires Filesystem snapshot content")
	}
	classes, err := r.Kube.ListVolumeSnapshotClasses(ctx)
	if err != nil {
		return pvc, err
	}
	storage, err := r.Kube.ListStorageClasses(ctx)
	if err != nil {
		return pvc, err
	}
	var driver string
	for _, c := range classes {
		if c.Name == *snap.Spec.VolumeSnapshotClassName {
			driver = c.Driver
		}
	}
	match := false
	for _, c := range storage {
		if c.Name == class && c.Provisioner == driver && driver != "" {
			match = true
		}
	}
	if !match {
		return pvc, fmt.Errorf("restore storage class does not match snapshot driver")
	}
	pvc.Metadata.Name = v.ClaimName
	pvc.Spec.StorageClassName = &class
	pvc.Spec.VolumeMode = &mode
	pvc.Status.Phase = "Bound"
	return pvc, nil
}
