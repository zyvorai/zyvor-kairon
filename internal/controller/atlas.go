// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	atlas "github.com/zyvorai/atlas/clients/go"

	"github.com/zyvorai/kairon/internal/model"
)

// AtlasConfig wires kairon-controller to an Atlas storage gateway. A nil
// Client disables provisioning: Machines with spec.volumes[].atlas stay
// Pending with an explanatory message, and deletions with the Atlas
// finalizer still wait for a Client before releasing volumes.
type AtlasConfig struct {
	Client        *atlas.Client
	TenantID      string
	DefaultPolicy string
	// BackupBucketID is the Atlas S3 bucket a MachineBackup uses when its
	// spec.atlas.bucketID is empty.
	BackupBucketID string
}

func hasAtlasVolumes(m model.Machine) bool {
	for _, v := range m.Spec.Volumes {
		if v.Atlas != nil {
			return true
		}
	}
	return false
}

// validateAtlasVolume checks the parts of an Atlas volume source the CRD
// schema can't express.
func validateAtlasVolume(index int, v model.MachineVolume, st model.AtlasVolumeState) error {
	src := v.Atlas
	if _, err := model.ParseBytes(src.Size); err != nil {
		return fmt.Errorf("volume %q: atlas.size: %w", v.Name, err)
	}
	switch src.EffectiveMode() {
	case model.AtlasModePVC:
		if v.ClaimName != "" && v.ClaimName != st.ClaimName {
			return fmt.Errorf("volume %q: claimName must be empty when atlas provisions the PVC", v.Name)
		}
	case model.AtlasModeRBD:
		if v.ClaimName != "" {
			return fmt.Errorf("volume %q: claimName is not used with atlas.mode=rbd", v.Name)
		}
		if index != 0 {
			return fmt.Errorf("volume %q: atlas.mode=rbd is only for the boot disk (volumes[0])", v.Name)
		}
	default:
		return fmt.Errorf("volume %q: atlas.mode %q must be pvc or rbd", v.Name, src.Mode)
	}
	return nil
}

// reconcileAtlasVolumes provisions and releases Atlas volumes. It never
// blocks on Atlas jobs: a create records the job id and later ticks poll it.
// It returns the machines with any metadata/spec it patched applied, and the
// set of namespace/name keys that must not be scheduled yet, with a reason.
func (c *Controller) reconcileAtlasVolumes(ctx context.Context, machines []model.Machine) ([]model.Machine, map[string]string) {
	notReady := map[string]string{}
	for i := range machines {
		m := &machines[i]
		reason, err := c.reconcileMachineAtlasVolumes(ctx, m)
		if err != nil {
			c.Log.Error("atlas volume reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("atlasvolumes")
			}
			if reason == "" {
				reason = err.Error()
			}
		}
		if reason != "" {
			notReady[m.Namespace()+"/"+m.Metadata.Name] = reason
		}
	}
	return machines, notReady
}

func (c *Controller) reconcileMachineAtlasVolumes(ctx context.Context, m *model.Machine) (string, error) {
	hasFinalizer := model.HasFinalizerList(m.Metadata.Finalizers, model.FinalizerAtlasVolumes)
	if m.Metadata.DeletionTimestamp != nil {
		if !hasFinalizer {
			return "", nil
		}
		return "", c.releaseAtlasVolumes(ctx, m)
	}
	if !hasAtlasVolumes(*m) {
		return "", nil
	}
	if c.Atlas.Client == nil {
		return "spec.volumes[].atlas requires kairon-controller --atlas-url", nil
	}
	if !hasFinalizer {
		meta := model.AddFinalizerMeta(m.Metadata.Finalizers, model.FinalizerAtlasVolumes, m.Metadata.ResourceVersion)
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": meta}); err != nil {
			return "", fmt.Errorf("add atlas-volumes finalizer: %w", err)
		}
		m.Metadata.Finalizers = meta["finalizers"].([]string)
		m.Metadata.ResourceVersion = "" // the patch moved it; later guarded writes in this pass use a fresh listing
	}

	states := model.AtlasVolumeStates(*m)
	before, _ := json.Marshal(states)
	var reasons []string
	var reconcileErr error
	for i, v := range m.Spec.Volumes {
		if v.Atlas == nil {
			continue
		}
		st := states[v.Name]
		if err := validateAtlasVolume(i, v, st); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		next, err := c.stepAtlasVolume(ctx, *m, i, v, st)
		if next != st {
			states[v.Name] = next
		}
		if err != nil {
			reconcileErr = err
			reasons = append(reasons, fmt.Sprintf("volume %q: %v", v.Name, err))
			continue
		}
		switch next.Phase {
		case model.AtlasPhaseReady:
		case model.AtlasPhaseFailed:
			reasons = append(reasons, fmt.Sprintf("volume %q: atlas provisioning failed: %s", v.Name, next.Message))
		default:
			reasons = append(reasons, fmt.Sprintf("volume %q: waiting for atlas (%s)", v.Name, next.Phase))
		}
	}

	if after, _ := json.Marshal(states); string(after) != string(before) {
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{model.AnnotationAtlasVolumes: string(after)}},
		}); err != nil {
			return "", fmt.Errorf("record atlas volume state: %w", err)
		}
		if m.Metadata.Annotations == nil {
			m.Metadata.Annotations = map[string]string{}
		}
		m.Metadata.Annotations[model.AnnotationAtlasVolumes] = string(after)
	}

	if err := c.syncAtlasClaimNames(ctx, m, states); err != nil {
		return "", err
	}

	if len(reasons) == 0 {
		return "", reconcileErr
	}
	reason := strings.Join(reasons, "; ")
	if m.Spec.NodeName == "" && (m.Status.Message != reason || m.Status.Phase != "Pending") {
		status := m.Status
		status.Phase = "Pending"
		status.Message = reason
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Scheduled", Status: "False", Reason: "VolumesNotReady", Message: reason,
		})
		if err := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
			c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		} else {
			m.Status = status
		}
	}
	return reason, reconcileErr
}

// stepAtlasVolume advances one volume by at most one Atlas call per phase.
func (c *Controller) stepAtlasVolume(ctx context.Context, m model.Machine, index int, v model.MachineVolume, st model.AtlasVolumeState) (model.AtlasVolumeState, error) {
	switch {
	case st.Phase == model.AtlasPhaseReady, st.Phase == model.AtlasPhaseFailed:
		return st, nil
	case st.VolumeID == "":
		return c.createAtlasVolume(ctx, m, index, v)
	case st.JobID != "":
		job, err := c.Atlas.Client.GetJob(ctx, st.JobID)
		if err != nil {
			return st, fmt.Errorf("get atlas job %s: %w", st.JobID, err)
		}
		switch job.State {
		case atlas.JobSucceeded:
			st.JobID = ""
		case atlas.JobFailed:
			st.Phase = model.AtlasPhaseFailed
			st.Message = "job " + st.JobID + " failed"
			if job.Error != nil {
				st.Message += ": " + *job.Error
			}
			return st, nil
		default:
			return st, nil
		}
	}
	return c.resolveAtlasVolume(ctx, st)
}

func (c *Controller) createAtlasVolume(ctx context.Context, m model.Machine, index int, v model.MachineVolume) (model.AtlasVolumeState, error) {
	src := v.Atlas
	size, _ := model.ParseBytes(src.Size)
	name := model.AtlasVolumeName(m.Namespace(), m.Metadata.Name, v.Name)
	mode := src.EffectiveMode()
	st := model.AtlasVolumeState{Mode: mode, Phase: model.AtlasPhaseProvisioning}

	var acc atlas.Accepted
	var err error
	if mode == model.AtlasModeRBD {
		acc, err = c.Atlas.Client.CreateRBDImage(ctx, atlas.CreateRBDImageRequest{
			Name: name, SizeBytes: size, Pool: src.Pool, TenantID: c.Atlas.TenantID,
		})
	} else {
		policy := src.Policy
		if policy == "" {
			policy = c.Atlas.DefaultPolicy
		}
		role := "data_disk"
		if index == 0 {
			role = "root_disk"
		}
		acc, err = c.Atlas.Client.CreateVolume(ctx, atlas.CreateVolumeRequest{
			TenantID:  c.Atlas.TenantID,
			Name:      name,
			SizeBytes: size,
			Kind:      atlas.KindBlock,
			Policy:    policy,
			Pool:      src.Pool,
			Owner: &atlas.Owner{
				Product:      "kairon",
				ResourceType: "machine",
				ResourceID:   m.Namespace() + "/" + m.Metadata.Name,
				Role:         role,
			},
			Kubernetes: &atlas.KubernetesOpts{
				BackendID:    src.BackendID,
				Namespace:    m.Namespace(),
				CreatePVC:    true,
				AccessModes:  []string{"ReadWriteOnce"},
				VolumeMode:   "Filesystem",
				StorageClass: src.StorageClass,
			},
		})
	}
	if err != nil {
		var apiErr *atlas.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != 429 {
			st.Phase = model.AtlasPhaseFailed
			st.Message = err.Error()
			return st, nil
		}
		return model.AtlasVolumeState{}, fmt.Errorf("create atlas volume %s: %w", name, err)
	}
	st.VolumeID = acc.Resource.VolumeID
	st.JobID = acc.JobID
	if acc.Resource.RBD != "" {
		st.NativeID = "rbd:" + acc.Resource.RBD
	}
	if acc.Resource.PVC != "" {
		st.ClaimName = acc.Resource.PVC
	}
	if st.VolumeID == "" {
		return model.AtlasVolumeState{}, fmt.Errorf("create atlas volume %s: response has no volume_id", name)
	}
	if st.JobID != "" {
		return st, nil
	}
	return c.resolveAtlasVolume(ctx, st)
}

// resolveAtlasVolume reads back a volume whose job finished and marks it
// Ready once the backend identity the node needs is known.
func (c *Controller) resolveAtlasVolume(ctx context.Context, st model.AtlasVolumeState) (model.AtlasVolumeState, error) {
	vol, err := c.Atlas.Client.GetVolume(ctx, st.VolumeID)
	if err != nil {
		return st, fmt.Errorf("get atlas volume %s: %w", st.VolumeID, err)
	}
	if id := vol.NativeID(); id != "" {
		st.NativeID = id
	}
	if vol.PVCName != nil && *vol.PVCName != "" {
		st.ClaimName = *vol.PVCName
	}
	switch st.Mode {
	case model.AtlasModeRBD:
		if _, _, ok := atlas.ParseRBD(st.NativeID); !ok {
			return st, nil
		}
	default:
		if st.ClaimName == "" {
			return st, nil
		}
	}
	st.Phase = model.AtlasPhaseReady
	st.Message = ""
	return st, nil
}

// syncAtlasClaimNames writes Atlas-created PVC names into spec.volumes so
// kairon-node's existing claim path consumes them unchanged.
func (c *Controller) syncAtlasClaimNames(ctx context.Context, m *model.Machine, states map[string]model.AtlasVolumeState) error {
	volumes := append([]model.MachineVolume(nil), m.Spec.Volumes...)
	changed := false
	for i, v := range volumes {
		if v.Atlas == nil || v.Atlas.EffectiveMode() != model.AtlasModePVC {
			continue
		}
		st := states[v.Name]
		if st.Phase == model.AtlasPhaseReady && st.ClaimName != "" && v.ClaimName == "" {
			volumes[i].ClaimName = st.ClaimName
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
		"spec": map[string]any{"volumes": volumes},
	}); err != nil {
		return fmt.Errorf("patch atlas claimNames: %w", err)
	}
	m.Spec.Volumes = volumes
	return nil
}

// stepAtlasDelete issues or polls one volume's delete. gone is true once
// Atlas reports the delete finished or the volume no longer exists; a failed
// delete job is cleared so the next tick re-issues the delete.
func (c *Controller) stepAtlasDelete(ctx context.Context, st model.AtlasVolumeState) (model.AtlasVolumeState, bool, error) {
	if st.Phase == model.AtlasPhaseDeleting && st.JobID != "" {
		job, err := c.Atlas.Client.GetJob(ctx, st.JobID)
		if err != nil {
			return st, false, fmt.Errorf("get delete job %s: %w", st.JobID, err)
		}
		switch job.State {
		case atlas.JobSucceeded:
			return st, true, nil
		case atlas.JobFailed:
			msg := "delete job " + st.JobID + " failed"
			if job.Error != nil {
				msg += ": " + *job.Error
			}
			st.JobID, st.Message = "", msg
			return st, false, errors.New(msg)
		default:
			return st, false, nil
		}
	}
	var acc atlas.Accepted
	var err error
	if pool, image, ok := atlas.ParseRBD(st.NativeID); ok && st.Mode == model.AtlasModeRBD {
		acc, err = c.Atlas.Client.DeleteRBDImage(ctx, pool, image)
	} else {
		acc, err = c.Atlas.Client.DeleteVolume(ctx, st.VolumeID)
	}
	switch {
	case atlas.IsNotFound(err):
		return st, true, nil
	case err != nil:
		return st, false, err
	case acc.Done || acc.JobID == "":
		return st, true, nil
	}
	st.Phase, st.JobID, st.Message = model.AtlasPhaseDeleting, acc.JobID, ""
	return st, false, nil
}

// releaseAtlasVolumes deletes non-retained Atlas volumes once kairon-node has
// torn down the runtime (its runtime-cleanup finalizer is gone).
func (c *Controller) releaseAtlasVolumes(ctx context.Context, m *model.Machine) error {
	if model.HasFinalizerList(m.Metadata.Finalizers, model.Finalizer) {
		return nil
	}
	if c.Atlas.Client == nil {
		return fmt.Errorf("atlas-volumes finalizer present but kairon-controller has no --atlas-url")
	}
	retain := map[string]bool{}
	for _, v := range m.Spec.Volumes {
		if v.Atlas != nil && v.Atlas.Retain {
			retain[v.Name] = true
		}
	}
	states := model.AtlasVolumeStates(*m)
	before, _ := json.Marshal(states)
	pending := 0
	var stepErr error
	for name, st := range states {
		if st.VolumeID == "" || retain[name] {
			continue
		}
		next, gone, err := c.stepAtlasDelete(ctx, st)
		switch {
		case gone:
			delete(states, name)
		default:
			states[name] = next
			pending++
		}
		if err != nil {
			stepErr = fmt.Errorf("delete atlas volume %s (%s): %w", name, st.VolumeID, err)
		}
	}
	if after, _ := json.Marshal(states); string(after) != string(before) {
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{model.AnnotationAtlasVolumes: string(after)}},
		}); err != nil {
			return fmt.Errorf("record atlas volume state: %w", err)
		}
		if m.Metadata.Annotations == nil {
			m.Metadata.Annotations = map[string]string{}
		}
		m.Metadata.Annotations[model.AnnotationAtlasVolumes] = string(after)
	}
	if stepErr != nil {
		return stepErr
	}
	if pending > 0 {
		return nil
	}
	finals := model.RemoveFinalizer(m.Metadata.Finalizers, model.FinalizerAtlasVolumes)
	if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
		"metadata": map[string]any{"finalizers": finals},
	}); err != nil {
		return err
	}
	m.Metadata.Finalizers = finals
	return nil
}
