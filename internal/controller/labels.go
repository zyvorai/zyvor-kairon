// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"

	"github.com/zyvorai/kairon/internal/model"
)

// syncAssignedNodeLabels keeps metadata.labels[AssignedNodeLabel] aligned
// with spec.nodeName for every Machine. Covers upgrade of pre-label
// assignments and any path that set nodeName without the label.
func (c *Controller) syncAssignedNodeLabels(ctx context.Context, machines []model.Machine) {
	for _, m := range machines {
		want := m.Spec.NodeName
		got := model.AssignedNodeLabelValue(m.Metadata.Labels)
		if want == got {
			continue
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, model.MetadataLabelsPatchForAssignedNode(want)); err != nil {
			c.Log.Error("assigned-node label sync failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	}
}

// syncMigrationSourceLabels keeps MigrationSourceNodeLabel aligned with
// status.sourceNode so node agents can list only their migrations.
// Terminal migrations no longer need the label for agent work; clear it
// when sourceNode is empty and otherwise keep it for diagnostics.
func (c *Controller) syncMigrationSourceLabels(ctx context.Context, migrations []model.MachineMigration) {
	for _, mig := range migrations {
		want := mig.Status.SourceNode
		if isTerminalMigrationPhase(mig.Status.Phase) {
			// Leave whatever label is present; agents ignore terminal
			// migrations anyway. Avoid write storms on historical objects.
			continue
		}
		got := ""
		if mig.Metadata.Labels != nil {
			got = mig.Metadata.Labels[model.MigrationSourceNodeLabel]
		}
		if want == got {
			continue
		}
		if err := c.ensureMigrationSourceLabel(ctx, mig, want); err != nil {
			c.Log.Error("migration source-node label sync failed", "namespace", mig.Namespace(), "migration", mig.Metadata.Name, "error", err)
		}
	}
}

func (c *Controller) ensureMigrationSourceLabel(ctx context.Context, migration model.MachineMigration, sourceNode string) error {
	got := ""
	if migration.Metadata.Labels != nil {
		got = migration.Metadata.Labels[model.MigrationSourceNodeLabel]
	}
	if got == sourceNode {
		return nil
	}
	return c.Kube.PatchMachineMigration(ctx, migration.Namespace(), migration.Metadata.Name, model.MetadataLabelsPatchForMigrationSource(sourceNode))
}
