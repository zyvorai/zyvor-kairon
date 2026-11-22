// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Package fencing is the one place a Machine is fenced off a dead node:
// spec.nodeName and the runtime status are cleared and the Fenced
// condition is set, so the normal scheduler places it again. Used by
// `kaironctl fence` and by kairon-controller's stale-node evacuation.
package fencing

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// AnnotationNodeFenced on a Node is an operator's (or an out-of-band
// power-fencing tool's) attestation that the node is powered off or
// otherwise cannot run VMs any more; the value is the evidence.
const AnnotationNodeFenced = "kairon.zyvor.dev/node-fenced"

// AnnotationEvacuate=true on a Machine opts it into automatic fencing
// once its node carries AnnotationNodeFenced.
const AnnotationEvacuate = "kairon.zyvor.dev/evacuate"

// Patcher is the slice of kube.Client fencing needs.
type Patcher interface {
	PatchMachine(ctx context.Context, ns, name string, patch map[string]any) error
	PatchMachineStatus(ctx context.Context, ns, name string, status model.MachineStatus) error
}

// Fence clears m's node assignment and runtime status and records why in
// the Fenced condition (condReason is its machine-readable Reason). The
// runtime-cleanup finalizer goes too: only the old node's kairon-node
// removes it, so a fenced Machine deleted before it is placed again would
// otherwise hang; the next node adds it back.
func Fence(ctx context.Context, kc Patcher, m model.Machine, condReason, message string) error {
	meta := map[string]any{"labels": map[string]any{model.AssignedNodeLabel: nil}}
	if model.HasFinalizer(m, model.Finalizer) {
		meta["finalizers"] = model.RemoveFinalizer(m.Metadata.Finalizers, model.Finalizer)
	}
	if err := kc.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
		"spec":     map[string]any{"nodeName": ""},
		"metadata": meta,
	}); err != nil {
		return fmt.Errorf("clear spec.nodeName on %s/%s: %w", m.Namespace(), m.Metadata.Name, err)
	}
	status := m.Status
	status.Phase = ""
	status.NodeName = ""
	status.RuntimeID = ""
	status.GuestIP = ""
	status.GuestIPs = nil
	status.Network = nil
	status.AppliedVCPUs = 0
	status.AppliedMemoryMiB = 0
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: model.ConditionFenced, Status: "True", Reason: condReason, Message: message,
	})
	if err := kc.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
		return fmt.Errorf("clear runtime status on %s/%s: %w", m.Namespace(), m.Metadata.Name, err)
	}
	return nil
}
