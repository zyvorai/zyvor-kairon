// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// resolveImageRefs pins spec.image.imageRef and spec.cdroms[].imageRef to
// the referenced MachineImage's source and digest, exactly once per entry
// (an entry that already has a source is left alone), and fills an empty
// spec.resources / spec.image.diskSize from the image's defaults. Like
// resolveInstanceTypes it mutates machines in place so this tick's quota
// and scheduling see the result, and patches the API object so kairon-node
// boots the pinned bytes.
//
// The returned map holds Machines that can't be resolved yet (missing or
// invalid image, wrong kind); the caller keeps them out of scheduling so a
// node never sees an unresolved reference.
func (c *Controller) resolveImageRefs(ctx context.Context, machines []model.Machine) ([]model.Machine, map[string]string) {
	waiting := map[string]string{}
	needs := false
	for _, m := range machines {
		if machineNeedsImageRef(m) {
			needs = true
			break
		}
	}
	if !needs {
		return machines, waiting
	}
	images, err := c.Kube.ListMachineImages(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("list machine images failed", "error", err)
	}
	byName := map[string]model.MachineImage{}
	for _, img := range images {
		byName[img.Metadata.Name] = img
	}
	for i := range machines {
		m := &machines[i]
		if !machineNeedsImageRef(*m) || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		key := m.Namespace() + "/" + m.Metadata.Name
		patch, reason := applyImageRefs(m, byName)
		if reason != "" {
			waiting[key] = reason
			c.markImageWaiting(ctx, *m, reason)
			continue
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"spec": patch}); err != nil {
			c.Log.Error("patch resolved machine image failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			waiting[key] = "resolving MachineImage: " + err.Error()
			continue
		}
		c.Log.Info("resolved machine image", "namespace", m.Namespace(), "machine", m.Metadata.Name, "image", m.Spec.Image.ImageRef, "digest", m.Spec.Image.Digest)
	}
	return machines, waiting
}

func machineNeedsImageRef(m model.Machine) bool {
	if m.Spec.Image.ImageRef != "" && m.Spec.Image.Source == nil {
		return true
	}
	for _, cd := range m.Spec.Cdroms {
		if cd.ImageRef != "" && cd.Source == nil {
			return true
		}
	}
	return false
}

// applyImageRefs resolves m in place and returns the spec merge patch, or a
// reason the Machine has to wait. All-or-nothing: nothing is applied unless
// every reference resolves.
func applyImageRefs(m *model.Machine, byName map[string]model.MachineImage) (map[string]any, string) {
	lookup := func(name, wantKind string) (model.MachineImage, string) {
		img, ok := byName[name]
		if !ok {
			return img, fmt.Sprintf("waiting for MachineImage %q", name)
		}
		if err := img.Spec.Validate(); err != nil {
			return img, fmt.Sprintf("MachineImage %q is invalid: %v", name, err)
		}
		if k := img.Spec.EffectiveKind(); k != wantKind {
			return img, fmt.Sprintf("MachineImage %q is kind %s, need %s", name, k, wantKind)
		}
		return img, ""
	}

	image := m.Spec.Image
	resources := m.Spec.Resources
	resourcesChanged := false
	if image.ImageRef != "" && image.Source == nil {
		img, reason := lookup(image.ImageRef, model.MachineImageKindDisk)
		if reason != "" {
			return nil, reason
		}
		src := img.Spec.Source
		image.Source, image.Digest = &src, img.Spec.Digest
		if image.DiskSize == "" {
			image.DiskSize = img.Spec.Defaults.DiskSize
		}
		if m.Spec.InstanceTypeName == "" && resources.CPU == "" && resources.Memory == "" {
			resources.CPU, resources.Memory = img.Spec.Defaults.CPU, img.Spec.Defaults.Memory
			resourcesChanged = resources.CPU != "" || resources.Memory != ""
		}
	}
	cdroms := append([]model.MachineCdrom(nil), m.Spec.Cdroms...)
	cdromsChanged := false
	for i, cd := range cdroms {
		if cd.ImageRef == "" || cd.Source != nil {
			continue
		}
		img, reason := lookup(cd.ImageRef, model.MachineImageKindISO)
		if reason != "" {
			return nil, fmt.Sprintf("spec.cdroms[%d]: %s", i, reason)
		}
		src := img.Spec.Source
		cdroms[i].Source, cdroms[i].Digest = &src, img.Spec.Digest
		cdromsChanged = true
	}

	patch := map[string]any{}
	if image.Source != m.Spec.Image.Source || image.DiskSize != m.Spec.Image.DiskSize {
		img := map[string]any{"source": image.Source, "digest": image.Digest}
		if image.DiskSize != "" {
			img["diskSize"] = image.DiskSize
		}
		patch["image"] = img
	}
	if resourcesChanged {
		patch["resources"] = map[string]any{"cpu": resources.CPU, "memory": resources.Memory}
	}
	if cdromsChanged {
		patch["cdroms"] = cdroms
	}
	m.Spec.Image, m.Spec.Resources, m.Spec.Cdroms = image, resources, cdroms
	return patch, ""
}

func (c *Controller) markImageWaiting(ctx context.Context, m model.Machine, reason string) {
	if m.Status.Message == reason && m.Status.Phase == "Pending" {
		return
	}
	status := m.Status
	status.Phase = "Pending"
	status.Message = reason
	if err := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
		c.Log.Error("patch machine image waiting status failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}
