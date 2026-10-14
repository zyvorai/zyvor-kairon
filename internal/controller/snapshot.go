// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// quiesceFreezeTimeout bounds how long reconcileSnapshot waits for
// kairon-node to confirm a guest freeze before giving up and falling back
// to a crash-consistent snapshot -- a guest-agent that's unreachable
// (guest not booted with it, network partition, guest hung) must never
// block a snapshot forever. There is no equivalent timeout on the thaw
// side -- see awaitGuestThaw's own doc comment for why.
const quiesceFreezeTimeout = 30 * time.Second

// reconcileSnapshot drives a MachineSnapshot to Succeeded, and -- when
// the target Machine has spec.guestAgent.enabled -- coordinates a real
// guest filesystem freeze/thaw around the underlying VolumeSnapshot
// creation for an application-consistent snapshot instead of a merely
// crash-consistent one. kairon-controller has no direct network path to
// a Machine's FluxVM instance (only the node it's scheduled on does), so
// this coordinates with kairon-node via a request/response annotation
// pair on the Machine itself (AnnotationQuiesceRequest/
// AnnotationQuiesceStatus, internal/model) rather than a new RPC of its
// own -- see internal/agent/quiesce.go for kairon-node's own half.
func (c *Controller) reconcileSnapshot(ctx context.Context, snapshot model.MachineSnapshot, machines map[string]model.Machine) error {
	if snapshot.Metadata.DeletionTimestamp != nil {
		return c.reconcileSnapshotDeletion(ctx, snapshot, machines)
	}
	if snapshot.Status.Phase == "Succeeded" || snapshot.Status.Phase == "Failed" {
		return nil
	}
	if strings.TrimSpace(snapshot.Spec.MachineName) == "" {
		return fmt.Errorf("spec.machineName is required")
	}
	machine, ok := machines[snapshot.Namespace()+"/"+snapshot.Spec.MachineName]
	if !ok {
		return fmt.Errorf("machine %s/%s not found", snapshot.Namespace(), snapshot.Spec.MachineName)
	}
	if len(machine.Spec.Volumes) == 0 {
		return fmt.Errorf("machine has no PVC-backed spec.volumes to snapshot")
	}
	if snapshot.Status.SourceSpec == nil {
		// Carried by every status write below, which copy snapshot.Status.
		source := machine.Spec
		snapshot.Status.SourceSpec = &source
	}
	volumes, err := snapshotVolumes(snapshot, machine)
	if err != nil {
		return err
	}
	machine.Spec.Volumes = volumes

	quiesce := machine.Spec.GuestAgent.Enabled
	switch {
	case quiesce && snapshot.Status.Phase == "":
		return c.requestGuestFreeze(ctx, snapshot, machine)
	case quiesce && snapshot.Status.Phase == "Freezing":
		return c.awaitGuestFreeze(ctx, snapshot, machine)
	case quiesce && snapshot.Status.Phase == "Thawing":
		return c.awaitGuestThaw(ctx, snapshot, machine)
	}

	return c.reconcileVolumeSnapshots(ctx, snapshot, machine)
}

// requestGuestFreeze is reconcileSnapshot's first step for a
// guestAgent-enabled Machine: patches AnnotationQuiesceRequest onto the
// Machine (kairon-node's own reconcile picks this up independently, see
// internal/agent/quiesce.go), and parks the snapshot in a new Freezing
// phase until awaitGuestFreeze sees it confirmed or times out.
func (c *Controller) requestGuestFreeze(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine) error {
	// Guard deletion before ever asking a guest to freeze -- without this,
	// deleting the MachineSnapshot object while it holds the guest frozen
	// (Freezing/Thawing) would leave nothing to ever clear
	// AnnotationQuiesceRequest, and kairon-node's own reconcileGuestQuiesce
	// only thaws in response to that annotation being cleared. See
	// reconcileSnapshotDeletion below.
	if !model.HasFinalizerList(snapshot.Metadata.Finalizers, model.FinalizerSnapshotQuiesce) {
		finals := append(append([]string{}, snapshot.Metadata.Finalizers...), model.FinalizerSnapshotQuiesce)
		patch := map[string]any{"metadata": map[string]any{"finalizers": finals}}
		if err := c.Kube.PatchMachineSnapshot(ctx, snapshot.Namespace(), snapshot.Metadata.Name, patch); err != nil {
			return fmt.Errorf("add quiesce finalizer to snapshot %s/%s: %w", snapshot.Namespace(), snapshot.Metadata.Name, err)
		}
	}
	ref := model.FormatQuiesceRef(snapshot.Metadata.Name, time.Now())
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{model.AnnotationQuiesceRequest: ref}}}
	if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, patch); err != nil {
		return fmt.Errorf("request guest quiesce on machine %s/%s: %w", machine.Namespace(), machine.Metadata.Name, err)
	}
	status := snapshot.Status
	status.Phase = "Freezing"
	status.Message = "requested a guest filesystem freeze for an application-consistent snapshot"
	return c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status)
}

// awaitGuestFreeze checks whether kairon-node has confirmed the freeze
// this snapshot requested (AnnotationQuiesceStatus matching the same
// ref requestGuestFreeze set) -- if so, proceeds straight into creating
// the VolumeSnapshots while the guest is held frozen. If
// quiesceFreezeTimeout has elapsed with no confirmation (guest agent
// unreachable, guest not actually running it despite the spec flag, a
// slow/stuck freeze), gives up waiting, clears the request (so a late
// freeze from kairon-node doesn't land after the fact and get stuck with
// nothing left to thaw it), and falls back to an unquiesced,
// crash-consistent snapshot -- never blocks a snapshot forever on a
// guest agent that isn't cooperating.
func (c *Controller) awaitGuestFreeze(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine) error {
	requested, requestedAt, ok := model.ParseQuiesceRef(machine.Metadata.Annotations[model.AnnotationQuiesceRequest])
	confirmed, _, _ := model.ParseQuiesceRef(machine.Metadata.Annotations[model.AnnotationQuiesceStatus])
	if !ok || requested != snapshot.Metadata.Name {
		// Our own request annotation is gone or now names something
		// else (e.g. a different MachineSnapshot for the same Machine
		// raced ahead) -- re-request rather than assume anything.
		return c.requestGuestFreeze(ctx, snapshot, machine)
	}
	if confirmed == snapshot.Metadata.Name {
		return c.reconcileVolumeSnapshots(ctx, snapshot, machine)
	}
	if time.Since(requestedAt) < quiesceFreezeTimeout {
		return nil // keep waiting, retried next tick
	}
	c.Log.Warn("guest quiesce freeze timed out, falling back to a crash-consistent snapshot", "namespace", snapshot.Namespace(), "snapshot", snapshot.Metadata.Name, "machine", machine.Metadata.Name, "timeout", quiesceFreezeTimeout)
	clear := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationQuiesceRequest: nil}}}
	if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, clear); err != nil {
		return fmt.Errorf("clear timed-out guest quiesce request on machine %s/%s: %w", machine.Namespace(), machine.Metadata.Name, err)
	}
	status := snapshot.Status
	status.Message = fmt.Sprintf("guest quiesce freeze did not confirm within %s; proceeding with a crash-consistent snapshot", quiesceFreezeTimeout)
	if err := c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status); err != nil {
		return err
	}
	return c.reconcileVolumeSnapshots(ctx, snapshot, machine)
}

// requestGuestThaw is called once every VolumeSnapshot this pass needed
// to create has had its create call issued (not once they're ready to
// use -- holding the guest frozen through CSI's own, potentially slow,
// async provisioning would be an unbounded, guest-visible stall). Clears
// AnnotationQuiesceRequest, which kairon-node's own reconcile reads as
// "thaw now" (see internal/agent/quiesce.go) -- the request/response
// pair's other direction from requestGuestFreeze/awaitGuestFreeze above.
func (c *Controller) requestGuestThaw(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine, refs []model.VolumeSnapshotReference) error {
	clear := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationQuiesceRequest: nil}}}
	if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, clear); err != nil {
		return fmt.Errorf("request guest thaw on machine %s/%s: %w", machine.Namespace(), machine.Metadata.Name, err)
	}
	status := snapshot.Status
	status.VolumeSnapshots = refs
	status.Phase = "Thawing"
	status.Message = "VolumeSnapshots requested; waiting for the guest filesystem to thaw"
	return c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status)
}

// awaitGuestThaw waits for kairon-node to confirm the thaw (removing
// AnnotationQuiesceStatus) before letting the snapshot reach Succeeded --
// deliberately never gives up here, unlike awaitGuestFreeze's own
// timeout: a snapshot that quietly reports done while the guest is still
// frozen would leave an operator with no explanation for a hung VM, and a
// guest-fsfreeze-thaw retry is safe (idempotent) to keep attempting
// indefinitely, the same "never silently abandon" posture
// MachineMigration's own NeedsRecovery phase already has.
func (c *Controller) awaitGuestThaw(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine) error {
	if _, _, ok := model.ParseQuiesceRef(machine.Metadata.Annotations[model.AnnotationQuiesceStatus]); ok {
		return nil // kairon-node hasn't confirmed the thaw yet -- retried next tick, indefinitely
	}
	return c.reconcileVolumeSnapshots(ctx, snapshot, machine)
}

// reconcileVolumeSnapshots is the storage-level half of a MachineSnapshot
// -- creating/polling the real CSI VolumeSnapshots underneath it. Called
// directly (no quiesce) when the target Machine has no guest agent, and
// as the continuation of the freeze/thaw dance above when it does.
func (c *Controller) reconcileVolumeSnapshots(ctx context.Context, snapshot model.MachineSnapshot, machine model.Machine) error {
	refs := make([]model.VolumeSnapshotReference, 0, len(machine.Spec.Volumes))
	allReady := true
	created := false
	if err := c.ensureAtlasSnapshotFinalizer(ctx, &snapshot, machine); err != nil {
		return err
	}
	for _, volume := range machine.Spec.Volumes {
		if volume.Atlas != nil && c.Atlas.Client != nil {
			ref, made, err := c.stepAtlasSnapshot(ctx, snapshot, machine, volume)
			if made {
				created = true
			}
			if err != nil {
				return c.failSnapshot(ctx, snapshot, machine, append(refs, ref), err)
			}
			allReady = allReady && ref.ReadyToUse
			refs = append(refs, ref)
			continue
		}
		if strings.TrimSpace(volume.Name) == "" || strings.TrimSpace(volume.ClaimName) == "" {
			return fmt.Errorf("machine volume requires name and claimName")
		}
		name := snapshotVolumeName(snapshot.Metadata.Name, volume.Name)
		vs, err := c.Kube.GetVolumeSnapshot(ctx, snapshot.Namespace(), name)
		if err != nil {
			if !kube.IsNotFound(err) {
				return err
			}
			claim := volume.ClaimName
			vs = model.VolumeSnapshot{
				TypeMeta: model.TypeMeta{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshot"},
				Metadata: model.ObjectMeta{Name: name, Namespace: snapshot.Namespace(), Labels: map[string]string{
					"kairon.zyvor.dev/machine":  machine.Metadata.Name,
					"kairon.zyvor.dev/snapshot": snapshot.Metadata.Name,
				}},
				Spec: model.VolumeSnapshotSpec{Source: model.VolumeSnapshotSource{PersistentVolumeClaimName: &claim}},
			}
			if snapshot.Spec.VolumeSnapshotClassName != "" {
				className := snapshot.Spec.VolumeSnapshotClassName
				vs.Spec.VolumeSnapshotClassName = &className
			}
			vs, err = c.Kube.CreateVolumeSnapshot(ctx, snapshot.Namespace(), vs)
			if err != nil {
				return err
			}
			created = true
		}
		if vs.Status.Error != nil && vs.Status.Error.Message != nil && *vs.Status.Error.Message != "" {
			return fmt.Errorf("VolumeSnapshot %s failed: %s", name, *vs.Status.Error.Message)
		}
		ready := vs.Status.ReadyToUse != nil && *vs.Status.ReadyToUse
		if !ready {
			allReady = false
		}
		refs = append(refs, model.VolumeSnapshotReference{VolumeName: volume.Name, VolumeSnapshotName: name, ReadyToUse: ready})
	}

	if created && machine.Spec.GuestAgent.Enabled && snapshot.Status.Phase == "Freezing" {
		// Every VolumeSnapshot this pass needed has had its create call
		// issued -- thaw now rather than holding the guest frozen through
		// the (potentially slow, async) wait for CSI readiness below.
		return c.requestGuestThaw(ctx, snapshot, machine, refs)
	}
	if created && !allReady {
		// Persist Atlas job IDs right away so the next tick polls them
		// instead of starting another snapshot.
		status := snapshot.Status
		status.VolumeSnapshots = refs
		status.Phase = "Pending"
		status.Message = "waiting for volume snapshots"
		return c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status)
	}

	status := snapshot.Status
	status.VolumeSnapshots = refs
	status.ReadyToUse = allReady
	if allReady {
		status.Phase = "Succeeded"
		status.Message = "all volume snapshots are ready to use"
	} else {
		status.Phase = "Pending"
		status.Message = "waiting for volume snapshots"
	}
	return c.Kube.PatchMachineSnapshotStatus(ctx, snapshot.Namespace(), snapshot.Metadata.Name, status)
}

// reconcileSnapshotDeletion is reconcileSnapshot's counterpart to
// requestGuestFreeze's finalizer-add: refuses to let the MachineSnapshot
// object actually disappear while it may still be the one holding
// AnnotationQuiesceRequest/AnnotationQuiesceStatus on its target Machine,
// requesting a thaw and waiting for kairon-node's confirmation first --
// the same "never silently abandon a frozen guest" posture
// awaitGuestThaw already has for the non-deletion path. A snapshot that
// never reached requestGuestFreeze (no guestAgent, or hasn't gotten there
// yet) never has the finalizer and returns immediately, deleting exactly
// as before this existed.
func (c *Controller) reconcileSnapshotDeletion(ctx context.Context, snapshot model.MachineSnapshot, machines map[string]model.Machine) error {
	if model.HasFinalizerList(snapshot.Metadata.Finalizers, model.FinalizerAtlasSnapshots) {
		if err := c.releaseAtlasSnapshots(ctx, &snapshot); err != nil {
			return err
		}
	}
	if !model.HasFinalizerList(snapshot.Metadata.Finalizers, model.FinalizerSnapshotQuiesce) {
		return nil
	}
	if machine, ok := machines[snapshot.Namespace()+"/"+snapshot.Spec.MachineName]; ok {
		if requested, _, ok := model.ParseQuiesceRef(machine.Metadata.Annotations[model.AnnotationQuiesceRequest]); ok && requested == snapshot.Metadata.Name {
			clear := map[string]any{"metadata": map[string]any{"annotations": map[string]any{model.AnnotationQuiesceRequest: nil}}}
			if err := c.Kube.PatchMachine(ctx, machine.Namespace(), machine.Metadata.Name, clear); err != nil {
				return fmt.Errorf("request guest thaw before deleting snapshot %s/%s: %w", snapshot.Namespace(), snapshot.Metadata.Name, err)
			}
			return nil // wait for kairon-node's thaw confirmation next tick
		}
		if confirmed, _, ok := model.ParseQuiesceRef(machine.Metadata.Annotations[model.AnnotationQuiesceStatus]); ok && confirmed == snapshot.Metadata.Name {
			return nil // thaw requested but not yet confirmed -- retried indefinitely, never abandoned
		}
	}
	// Either the target Machine is gone too (nothing left to thaw), or the
	// guest is already confirmed thawed (or was never frozen on this
	// snapshot's behalf in the first place) -- safe to let deletion proceed.
	finals := model.RemoveFinalizer(snapshot.Metadata.Finalizers, model.FinalizerSnapshotQuiesce)
	patch := map[string]any{"metadata": map[string]any{"finalizers": finals}}
	return c.Kube.PatchMachineSnapshot(ctx, snapshot.Namespace(), snapshot.Metadata.Name, patch)
}

// snapshotVolumes returns the Machine volumes spec.volumeNames selects, or
// all of them when it is empty.
func snapshotVolumes(snapshot model.MachineSnapshot, machine model.Machine) ([]model.MachineVolume, error) {
	if len(snapshot.Spec.VolumeNames) == 0 {
		return machine.Spec.Volumes, nil
	}
	out := make([]model.MachineVolume, 0, len(snapshot.Spec.VolumeNames))
	for _, name := range snapshot.Spec.VolumeNames {
		found := false
		for _, v := range machine.Spec.Volumes {
			if v.Name == name {
				out = append(out, v)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("spec.volumeNames: machine %s has no volume %q", machine.Metadata.Name, name)
		}
	}
	return out, nil
}
