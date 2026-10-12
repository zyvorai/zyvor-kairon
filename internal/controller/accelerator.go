// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/topology"
)

const (
	// acceleratorCapacityLimit caps status.nodes[].fits per node.
	acceleratorCapacityLimit = 16
	// maxMachineDevices is the Machine CRD's spec.devices maxItems.
	maxMachineDevices = 8
)

func (c *Controller) listAcceleratorProfiles(ctx context.Context) []model.MachineAcceleratorProfile {
	profiles, err := c.Kube.ListMachineAcceleratorProfiles(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("list machine accelerator profiles failed", "error", err)
	}
	return profiles
}

// resolveAccelerators expands every unscheduled Machine's spec.accelerators
// into spec.devices once, like resolveInstanceTypes: the profile is copied
// in, so editing or deleting it later does not change the Machine. The
// returned map holds Machines that have to wait (missing or unusable
// profile); the caller keeps them out of scheduling.
func (c *Controller) resolveAccelerators(ctx context.Context, machines []model.Machine, profiles []model.MachineAcceleratorProfile) ([]model.Machine, map[string]string) {
	waiting := map[string]string{}
	byName := make(map[string]model.MachineAcceleratorProfile, len(profiles))
	for _, p := range profiles {
		byName[p.Metadata.Name] = p
	}
	for i := range machines {
		m := &machines[i]
		if m.AcceleratorsResolved() || m.Spec.NodeName != "" || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		key := m.Namespace() + "/" + m.Metadata.Name
		devices, record, reason := expandAccelerators(*m, byName)
		if reason != "" {
			waiting[key] = reason
			c.markAcceleratorWaiting(ctx, *m, reason)
			continue
		}
		patch := map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{model.AnnotationAccelerators: record}},
			"spec":     map[string]any{"devices": devices},
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
			c.Log.Error("patch expanded accelerators failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			waiting[key] = "expanding spec.accelerators: " + err.Error()
			continue
		}
		if m.Metadata.Annotations == nil {
			m.Metadata.Annotations = map[string]string{}
		}
		m.Metadata.Annotations[model.AnnotationAccelerators] = record
		m.Spec.Devices = devices
		c.Log.Info("expanded machine accelerators", "namespace", m.Namespace(), "machine", m.Metadata.Name, "profiles", record)
	}
	return machines, waiting
}

// expandAccelerators returns m's spec.devices with every accelerator's
// profile appended and the AnnotationAccelerators record, or why it cannot
// yet. Entries already in spec.devices are kept, so expanding twice is a
// no-op.
func expandAccelerators(m model.Machine, byName map[string]model.MachineAcceleratorProfile) ([]model.MachineDevice, string, string) {
	devices := append([]model.MachineDevice(nil), m.Spec.Devices...)
	have := map[string]bool{}
	for _, d := range devices {
		have[d.Name] = true
	}
	record := map[string]string{}
	for _, a := range m.Spec.Accelerators {
		p, ok := byName[a.Profile]
		if !ok {
			return nil, "", fmt.Sprintf("waiting for MachineAcceleratorProfile %q", a.Profile)
		}
		if _, msg := p.Spec.Usable(); msg != "" {
			return nil, "", fmt.Sprintf("MachineAcceleratorProfile %q cannot be used: %s", a.Profile, msg)
		}
		for _, d := range p.Spec.Devices(a.Name) {
			if !have[d.Name] {
				devices = append(devices, d)
				have[d.Name] = true
			}
		}
		record[a.Name] = fmt.Sprintf("%s@%d", p.Metadata.Name, p.Metadata.Generation)
	}
	if len(devices) > maxMachineDevices {
		return nil, "", fmt.Sprintf("expanding spec.accelerators: %d spec.devices entries, at most %d", len(devices), maxMachineDevices)
	}
	spec := m.Spec
	spec.Devices = devices
	if err := model.ValidateTopology(spec); err != nil {
		return nil, "", "expanding spec.accelerators: " + err.Error()
	}
	return devices, model.FormatAcceleratorRecord(record), ""
}

func (c *Controller) markAcceleratorWaiting(ctx context.Context, m model.Machine, reason string) {
	if m.Status.Message == reason && m.Status.Phase == "Pending" {
		return
	}
	status := m.Status
	status.Phase = "Pending"
	status.Message = reason
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Scheduled", Status: "False", Reason: model.ReasonAcceleratorProfileUnavailable, Message: reason,
	})
	if err := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
		c.Log.Error("patch machine accelerator waiting status failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
	}
}

// reconcileAcceleratorProfiles writes each profile's status: whether it is
// usable, how many more Machines with it each node's free devices could
// take, and how many Machines were expanded from it.
func (c *Controller) reconcileAcceleratorProfiles(ctx context.Context, profiles []model.MachineAcceleratorProfile, machines []model.Machine, nodes []model.Node, topologies map[string]model.NodeTopology) {
	if len(profiles) == 0 {
		if c.Metrics != nil {
			c.Metrics.ObserveAcceleratorProfiles(nil)
		}
		return
	}
	using := map[string]int{}
	for _, m := range machines {
		if m.Metadata.DeletionTimestamp != nil {
			continue
		}
		seen := map[string]bool{}
		for _, p := range model.AcceleratorProfilesOf(m) {
			if !seen[p] {
				seen[p] = true
				using[p]++
			}
		}
	}
	sorted := append([]model.Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Metadata.Name < sorted[j].Metadata.Name })
	graphs := map[string]topology.Graph{}
	for _, n := range sorted {
		if nt, ok := topologies[n.Metadata.Name]; ok && !n.Spec.Unschedulable {
			graphs[n.Metadata.Name] = topology.BuildGraph(nt, n, machines, model.Machine{})
		}
	}
	for i := range profiles {
		p := &profiles[i]
		st := acceleratorProfileStatus(*p, sorted, graphs, using[p.Metadata.Name])
		if reflect.DeepEqual(st, p.Status) {
			continue
		}
		if err := c.Kube.PatchMachineAcceleratorProfileStatus(ctx, p.Metadata.Name, st); err != nil {
			c.Log.Error("patch machine accelerator profile status failed", "profile", p.Metadata.Name, "error", err)
			continue
		}
		p.Status = st
	}
	if c.Metrics != nil {
		c.Metrics.ObserveAcceleratorProfiles(profiles)
	}
}

func acceleratorProfileStatus(p model.MachineAcceleratorProfile, nodes []model.Node, graphs map[string]topology.Graph, machines int) model.MachineAcceleratorProfileStatus {
	st := model.MachineAcceleratorProfileStatus{
		ObservedGeneration: p.Metadata.Generation,
		Conditions:         append([]model.Condition(nil), p.Status.Conditions...),
		Machines:           machines,
	}
	if reason, msg := p.Spec.Usable(); reason != "" {
		st.Conditions = model.SetCondition(st.Conditions, model.Condition{
			Type: model.AcceleratorProfileReady, Status: "False", Reason: reason, Message: msg,
		})
		st.Conditions = model.SetCondition(st.Conditions, model.Condition{
			Type: model.AcceleratorProfileAvailable, Status: "False", Reason: reason, Message: "the profile is not Ready",
		})
		return st
	}
	st.Conditions = model.SetCondition(st.Conditions, model.Condition{
		Type: model.AcceleratorProfileReady, Status: "True", Reason: model.AcceleratorProfileReasonValid,
	})
	reqs := p.Spec.Devices("accelerator")
	firstReason := "no node has a NodeTopology"
	fitting := 0
	for _, n := range nodes {
		g, ok := graphs[n.Metadata.Name]
		if !ok {
			continue
		}
		fits, reason := topology.DeviceCapacity(g, reqs, acceleratorCapacityLimit)
		nc := model.AcceleratorNodeCapacity{Node: n.Metadata.Name, Fits: fits}
		if fits == 0 {
			nc.Reason = reason
			if fitting == 0 && firstReason == "no node has a NodeTopology" {
				firstReason = n.Metadata.Name + ": " + reason
			}
		} else {
			fitting++
		}
		st.Capacity += fits
		st.Nodes = append(st.Nodes, nc)
	}
	if st.Capacity > 0 {
		st.Conditions = model.SetCondition(st.Conditions, model.Condition{
			Type: model.AcceleratorProfileAvailable, Status: "True", Reason: model.AcceleratorProfileReasonFits,
			Message: fmt.Sprintf("%d more Machines fit on %d nodes (devices only)", st.Capacity, fitting),
		})
	} else {
		st.Conditions = model.SetCondition(st.Conditions, model.Condition{
			Type: model.AcceleratorProfileAvailable, Status: "False", Reason: model.AcceleratorProfileReasonNoCapacity, Message: firstReason,
		})
	}
	return st
}
