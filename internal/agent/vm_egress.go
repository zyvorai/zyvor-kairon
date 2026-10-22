// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileEgress keeps FluxVM's outbound policy for the VM's QEMU process equal to
// spec.network.egress. A guest on user-mode networking has no host-visible interface, so this is
// the only policy that governs it.
//
// It writes only when FluxVM reports something different, and a running VM takes the change at
// once. Dropping the field clears the VM's own policy again, but only for a Machine kairon-node
// had set one on (AnnotationEgressManaged): a policy an operator put on FluxVM directly is left
// alone by a Machine that never asked for one.
func (a *Agent) reconcileEgress(ctx context.Context, m model.Machine, rec *fluxvm.Record) error {
	managed := m.Metadata.Annotations[model.AnnotationEgressManaged] == "true"
	want := fluxvm.WireEgress(m.Spec.Network.Egress)
	if want == nil && !managed {
		return nil
	}
	if normalizePhase(rec.Status) != "Running" {
		// A stopped VM picks the policy up from its create request or the next reconcile.
		return nil
	}
	id := rec.ID()
	if want == nil {
		if err := a.Flux.ClearVMEgress(ctx, id); err != nil {
			return fmt.Errorf("clear egress policy: %w", err)
		}
		return a.markEgressManaged(ctx, m, false)
	}
	if err := model.ValidateEgress(m.Spec.Network); err != nil {
		return err
	}
	got, source, err := a.Flux.GetVMEgress(ctx, id)
	if err != nil {
		return fmt.Errorf("read egress policy: %w", err)
	}
	if source == "vm" && got.Equivalent(*want) {
		if !managed {
			return a.markEgressManaged(ctx, m, true)
		}
		return nil
	}
	if err := a.Flux.SetVMEgress(ctx, id, *want); err != nil {
		return fmt.Errorf("set egress policy: %w", err)
	}
	return a.markEgressManaged(ctx, m, true)
}

func (a *Agent) markEgressManaged(ctx context.Context, m model.Machine, on bool) error {
	have := m.Metadata.Annotations[model.AnnotationEgressManaged] == "true"
	if have == on {
		return nil
	}
	var v any = nil // a null in a merge patch removes the annotation
	if on {
		v = "true"
	}
	return a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{model.AnnotationEgressManaged: v}},
	})
}
