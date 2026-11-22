// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/nodeliveness"
)

// detectUnreachableNodes keeps model.ConditionNodeUnreachable in sync with
// reality every reconcile tick.
//
// Primary signal: Kubernetes Node Ready (as before).
//
// Optional second signal (when NodeLivenessLeaseNamespace is set): a Ready
// node whose kairon-node liveness Lease is missing or stale is also treated
// as unreachable — catches a wedged agent on an otherwise-Ready node, the
// gap STATUS.md named. Fail-open on lease lookup errors (leave prior
// condition) so a transient apiserver blip does not mass-mark the fleet.
//
// Detection only — never auto-reschedule; operators use `kaironctl fence`.
func (c *Controller) detectUnreachableNodes(ctx context.Context, machines []model.Machine, nodes []model.Node) {
	readyNodes := map[string]bool{}
	for _, n := range nodes {
		if nodeReady(n) {
			readyNodes[n.Metadata.Name] = true
		}
	}
	leaseFresh := map[string]bool{} // only populated when lease ns configured
	if c.NodeLivenessLeaseNamespace != "" {
		for _, n := range nodes {
			lease, err := c.Kube.GetLease(ctx, c.NodeLivenessLeaseNamespace, nodeliveness.LeaseName(n.Metadata.Name))
			if err != nil {
				// Fail open: do not invent AgentLivenessStale on lookup error.
				continue
			}
			leaseFresh[n.Metadata.Name] = nodeliveness.IsFresh(lease, 0)
		}
	}
	for _, m := range machines {
		if m.Metadata.DeletionTimestamp != nil || m.Spec.NodeName == "" {
			continue
		}
		nodeName := m.Spec.NodeName
		unreachable := !readyNodes[nodeName]
		reason := "NodeNotReadyOrMissing"
		message := fmt.Sprintf("node %q is not Ready or no longer exists in the cluster; this Machine will NOT be automatically rescheduled -- see \"kaironctl fence\" only once you've confirmed out-of-band that the node is truly gone, not just unreachable", nodeName)
		if !unreachable && c.NodeLivenessLeaseNamespace != "" {
			fresh, seen := leaseFresh[nodeName]
			if seen && !fresh {
				unreachable = true
				reason = "AgentLivenessStale"
				message = fmt.Sprintf("node %q is Ready but kairon-node's liveness Lease is stale — agent may be wedged; this Machine will NOT be automatically rescheduled — use \"kaironctl fence\" only after confirming the agent/node is truly gone", nodeName)
			}
		}
		current, found := model.FindCondition(m.Status.Conditions, model.ConditionNodeUnreachable)
		currentlyTrue := found && current.Status == "True"
		if unreachable == currentlyTrue {
			continue
		}
		status, okReason, okMessage := "False", "NodeReady", fmt.Sprintf("node %q is Ready", nodeName)
		if unreachable {
			status, okReason, okMessage = "True", reason, message
		}
		newStatus := m.Status
		newStatus.Conditions = model.SetCondition(m.Status.Conditions, model.Condition{
			Type: model.ConditionNodeUnreachable, Status: status, Reason: okReason, Message: okMessage,
		})
		if err := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, newStatus); err != nil {
			c.Log.Error("machine status patch failed (node reachability)", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			continue
		}
		if unreachable {
			c.Log.Warn("machine's node is unreachable", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", nodeName, "reason", okReason)
		}
	}
}

// nodeReady deliberately duplicates internal/scheduler's own unexported
// ready() rather than importing it -- same "each consumer's own narrower
// copy" precedent this codebase already uses for isTerminalMigrationPhase,
// not a shared cross-package dependency worth taking for one boolean.
func nodeReady(n model.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
