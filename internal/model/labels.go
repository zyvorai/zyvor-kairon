// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

// AssignedNodeLabelSelector returns the labelSelector string that selects
// Machines assigned to nodeName.
func AssignedNodeLabelSelector(nodeName string) string {
	return AssignedNodeLabel + "=" + nodeName
}

// MigrationSourceNodeLabelSelector selects migrations whose source agent
// must drive the transfer.
func MigrationSourceNodeLabelSelector(nodeName string) string {
	return MigrationSourceNodeLabel + "=" + nodeName
}

// AssignedNodeLabelValue returns the current assigned-node label value, or
// empty if unset.
func AssignedNodeLabelValue(labels map[string]string) string {
	if labels == nil {
		return ""
	}
	return labels[AssignedNodeLabel]
}

// MetadataLabelsPatchForAssignedNode builds a merge-patch fragment that
// sets or clears AssignedNodeLabel to match nodeName (empty clears).
func MetadataLabelsPatchForAssignedNode(nodeName string) map[string]any {
	var value any = nodeName
	if nodeName == "" {
		value = nil
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{AssignedNodeLabel: value},
		},
	}
}

// MetadataLabelsPatchForMigrationSource builds a merge-patch fragment for
// MigrationSourceNodeLabel.
func MetadataLabelsPatchForMigrationSource(nodeName string) map[string]any {
	var value any = nodeName
	if nodeName == "" {
		value = nil
	}
	return map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{MigrationSourceNodeLabel: value},
		},
	}
}
