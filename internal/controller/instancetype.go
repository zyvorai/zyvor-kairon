// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// resolveInstanceTypes fills in Resources on every Machine that names an
// InstanceTypeName while Resources is still completely empty, from the
// matching MachineInstanceType (same namespace) or
// MachineClusterInstanceType (spec.instanceTypeKind) -- exactly once per
// Machine: a Machine whose Resources is already set (whether because an
// operator set it directly, or because a previous tick already resolved
// it here) is left untouched, matching this project's "creation-time-only"
// convention for every other field that only takes effect once (spec.image,
// spec.network.forwards, spec.cloudInit -- see docs/guides/machine-storage.md).
//
// Mutates machines in place (not just the API object) so this same tick's
// later steps -- quota tallying, scheduling -- see the resolved footprint
// immediately, rather than admitting a Machine at a false 0 CPU/memory
// footprint for one tick before its real resources are visible (quota's
// own machineFootprint silently treats an empty/invalid quantity as 0,
// so this isn't just a cosmetic staleness -- it would let a resolving
// Machine slip past a quota it should have been blocked by). The API
// object is also patched, so kairon-node and every other reader sees the
// same resolved spec.resources kairon-controller just computed, with no
// special-casing needed anywhere downstream.
//
// Preferences (spec.preferenceName) are applied in the same pass, also
// once: the applied preference is recorded in
// model.AnnotationPreferenceApplied.
func (c *Controller) resolveInstanceTypes(ctx context.Context, machines []model.Machine) []model.Machine {
	needsType, needsPref := false, false
	for _, m := range machines {
		needsType = needsType || machineNeedsInstanceType(m)
		needsPref = needsPref || machineNeedsPreference(m)
	}
	if needsType {
		machines = c.resolveInstanceTypeResources(ctx, machines)
	}
	if needsPref {
		machines = c.resolvePreferences(ctx, machines)
	}
	return machines
}

func (c *Controller) resolveInstanceTypeResources(ctx context.Context, machines []model.Machine) []model.Machine {
	byKey := map[string]model.ResourceSpec{}
	instanceTypes, err := c.Kube.ListMachineInstanceTypes(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("list machine instance types failed", "error", err)
		return machines
	}
	for _, it := range instanceTypes {
		byKey[instanceTypeKey(model.KindMachineInstanceType, it.Namespace(), it.Metadata.Name)] = it.Spec.Resources
	}
	if anyMachine(machines, func(m model.Machine) bool { return m.Spec.InstanceTypeKind == model.KindMachineClusterInstanceType }) {
		cluster, err := c.Kube.ListMachineClusterInstanceTypes(ctx)
		if err != nil && !kube.IsNotFound(err) {
			c.Log.Error("list machine cluster instance types failed", "error", err)
		}
		for _, it := range cluster {
			byKey[instanceTypeKey(model.KindMachineClusterInstanceType, "", it.Metadata.Name)] = it.Spec.Resources
		}
	}

	for i, m := range machines {
		if !machineNeedsInstanceType(m) {
			continue
		}
		res, ok := byKey[machineInstanceTypeKey(m)]
		if !ok {
			// Left unresolved -- surfaces downstream as a clear "cpu/memory
			// quantity is empty" error at FluxVM creation time, the same
			// failure mode a hand-typed empty spec.resources already has.
			// With the webhook on, a missing reference is refused at create.
			continue
		}
		machines[i].Spec.Resources = res
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"spec": map[string]any{"resources": res}}); err != nil {
			c.Log.Error("patch resolved instance type resources failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "instanceType", m.Spec.InstanceTypeName, "error", err)
			// Keep the in-memory resolution for this tick regardless --
			// retried against the API again next tick since Resources on
			// the persisted object is still empty.
			continue
		}
		c.Log.Info("resolved machine instance type", "namespace", m.Namespace(), "machine", m.Metadata.Name, "instanceType", m.Spec.InstanceTypeName, "cpu", res.CPU, "memory", res.Memory)
	}
	return machines
}

func (c *Controller) resolvePreferences(ctx context.Context, machines []model.Machine) []model.Machine {
	byKey := map[string]model.MachinePreferenceSpec{}
	prefs, err := c.Kube.ListMachinePreferences(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("list machine preferences failed", "error", err)
		return machines
	}
	for _, p := range prefs {
		byKey[instanceTypeKey(model.KindMachinePreference, p.Namespace(), p.Metadata.Name)] = p.Spec
	}
	if anyMachine(machines, func(m model.Machine) bool { return m.Spec.PreferenceKind == model.KindMachineClusterPreference }) {
		cluster, err := c.Kube.ListMachineClusterPreferences(ctx)
		if err != nil && !kube.IsNotFound(err) {
			c.Log.Error("list machine cluster preferences failed", "error", err)
		}
		for _, p := range cluster {
			byKey[instanceTypeKey(model.KindMachineClusterPreference, "", p.Metadata.Name)] = p.Spec
		}
	}

	for i, m := range machines {
		if !machineNeedsPreference(m) {
			continue
		}
		key := machinePreferenceKey(m)
		pref, ok := byKey[key]
		if !ok {
			continue
		}
		spec := m.Spec
		model.ApplyPreference(&spec, pref)
		applied := preferenceKind(m) + "/" + m.Spec.PreferenceName
		patch := map[string]any{
			"metadata": map[string]any{"annotations": map[string]string{model.AnnotationPreferenceApplied: applied}},
			"spec":     preferencePatch(m.Spec, spec),
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
			c.Log.Error("patch machine preference failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "preference", applied, "error", err)
			continue
		}
		machines[i].Spec = spec
		if machines[i].Metadata.Annotations == nil {
			machines[i].Metadata.Annotations = map[string]string{}
		}
		machines[i].Metadata.Annotations[model.AnnotationPreferenceApplied] = applied
		c.Log.Info("applied machine preference", "namespace", m.Namespace(), "machine", m.Metadata.Name, "preference", applied)
	}
	return machines
}

// preferencePatch is the merge patch for the spec fields ApplyPreference
// changed between before and after.
func preferencePatch(before, after model.MachineSpec) map[string]any {
	out := map[string]any{}
	if before.Runtime.Backend != after.Runtime.Backend {
		out["runtime"] = map[string]any{"backend": after.Runtime.Backend}
	}
	if before.Placement.Architecture != after.Placement.Architecture {
		out["placement"] = map[string]any{"architecture": after.Placement.Architecture}
	}
	sec := map[string]any{}
	if before.Security.SecureBoot != after.Security.SecureBoot {
		sec["secureBoot"] = after.Security.SecureBoot
	}
	if before.Security.TPM != after.Security.TPM {
		sec["tpm"] = after.Security.TPM
	}
	if len(sec) > 0 {
		out["security"] = sec
	}
	if before.GuestAgent.Enabled != after.GuestAgent.Enabled {
		out["guestAgent"] = map[string]any{"enabled": after.GuestAgent.Enabled}
	}
	return out
}

func machineNeedsInstanceType(m model.Machine) bool {
	return m.Spec.InstanceTypeName != "" && m.Spec.Resources.CPU == "" && m.Spec.Resources.Memory == ""
}

func machineNeedsPreference(m model.Machine) bool {
	return m.Spec.PreferenceName != "" && m.Metadata.Annotations[model.AnnotationPreferenceApplied] == ""
}

func instanceTypeKind(m model.Machine) string {
	if m.Spec.InstanceTypeKind == "" {
		return model.KindMachineInstanceType
	}
	return m.Spec.InstanceTypeKind
}

func preferenceKind(m model.Machine) string {
	if m.Spec.PreferenceKind == "" {
		return model.KindMachinePreference
	}
	return m.Spec.PreferenceKind
}

func instanceTypeKey(kind, ns, name string) string {
	return kind + "/" + ns + "/" + name
}

func machineInstanceTypeKey(m model.Machine) string {
	kind := instanceTypeKind(m)
	ns := m.Namespace()
	if kind == model.KindMachineClusterInstanceType {
		ns = ""
	}
	return instanceTypeKey(kind, ns, m.Spec.InstanceTypeName)
}

func machinePreferenceKey(m model.Machine) string {
	kind := preferenceKind(m)
	ns := m.Namespace()
	if kind == model.KindMachineClusterPreference {
		ns = ""
	}
	return instanceTypeKey(kind, ns, m.Spec.PreferenceName)
}

func anyMachine(machines []model.Machine, pred func(model.Machine) bool) bool {
	for _, m := range machines {
		if pred(m) {
			return true
		}
	}
	return false
}

// checkInstanceTypeRefs reports a missing instance type or preference a
// new Machine names. Lookup errors other than NotFound fail open.
func (c *Controller) checkInstanceTypeRefs(ctx context.Context, m model.Machine) string {
	if err := model.ValidateInstanceTypeRef(m.Spec); err != nil {
		return err.Error()
	}
	if m.Spec.InstanceTypeName != "" && m.Spec.Resources.CPU == "" && m.Spec.Resources.Memory == "" {
		var err error
		if instanceTypeKind(m) == model.KindMachineClusterInstanceType {
			_, err = c.Kube.GetMachineClusterInstanceType(ctx, m.Spec.InstanceTypeName)
		} else {
			_, err = c.Kube.GetMachineInstanceType(ctx, m.Namespace(), m.Spec.InstanceTypeName)
		}
		if kube.IsNotFound(err) {
			return instanceTypeKind(m) + " " + m.Spec.InstanceTypeName + " not found"
		}
	}
	if m.Spec.PreferenceName != "" {
		var err error
		if preferenceKind(m) == model.KindMachineClusterPreference {
			_, err = c.Kube.GetMachineClusterPreference(ctx, m.Spec.PreferenceName)
		} else {
			_, err = c.Kube.GetMachinePreference(ctx, m.Namespace(), m.Spec.PreferenceName)
		}
		if kube.IsNotFound(err) {
			return preferenceKind(m) + " " + m.Spec.PreferenceName + " not found"
		}
	}
	return ""
}
