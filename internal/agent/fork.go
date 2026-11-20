// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

// forkRuntime gives fork child m its runtime by forking the parent
// Machine's FluxVM VM. FluxVM names the child "{prefix}-1", so a child left
// behind by a tick whose status patch failed is found by that name and
// adopted instead of forked a second time.
func (a *Agent) forkRuntime(ctx context.Context, m model.Machine) (*fluxvm.Record, error) {
	childName := m.RuntimeName() + "-1"
	if rec, err := a.Flux.LookupByName(ctx, childName); err != nil || rec != nil {
		return rec, err
	}
	if len(m.Spec.Volumes) > 0 || len(m.Spec.Disks) > 0 || len(m.Spec.DeviceClaims) > 0 {
		return nil, fmt.Errorf("fork child %s/%s must not have volumes, disks or device claims: the child gets a copy-on-write copy of the parent's disk", m.Namespace(), m.Metadata.Name)
	}
	parentName := m.Metadata.Annotations[model.AnnotationForkFrom]
	parent, err := a.Kube.GetMachine(ctx, m.Namespace(), parentName)
	if err != nil {
		return nil, fmt.Errorf("fork parent %s/%s: %w", m.Namespace(), parentName, err)
	}
	if parent.Spec.NodeName != a.NodeName {
		return nil, fmt.Errorf("fork parent %s/%s runs on node %q, not %q: a fork child must be scheduled on its parent's node", m.Namespace(), parentName, parent.Spec.NodeName, a.NodeName)
	}
	src, err := a.current(ctx, parent)
	if err != nil {
		return nil, err
	}
	if src == nil || src.ID() == "" {
		return nil, fmt.Errorf("fork parent %s/%s has no running FluxVM runtime", m.Namespace(), parentName)
	}
	kids, err := a.Flux.Fork(ctx, src.ID(), 1, m.RuntimeName())
	if err != nil {
		return nil, fmt.Errorf("fork %s/%s: %w", m.Namespace(), parentName, err)
	}
	return &kids[0], nil
}
