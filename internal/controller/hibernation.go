// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// awaitingResume reports a hibernated Machine that is wanted Running again
// and has no resume node yet.
func awaitingResume(m model.Machine) bool {
	h := m.Status.Hibernation
	return h != nil && h.ResumeNode == "" && m.Metadata.DeletionTimestamp == nil &&
		m.Spec.NodeName != "" && m.DesiredPowerState() == "Running"
}

func withoutMachine(machines []model.Machine, m model.Machine) []model.Machine {
	out := make([]model.Machine, 0, len(machines))
	for _, x := range machines {
		if x.Namespace() == m.Namespace() && x.Metadata.Name == m.Metadata.Name {
			continue
		}
		out = append(out, x)
	}
	return out
}

// resumeCandidates are the ready nodes other than the Machine's own whose
// FluxVM can load its saved state, by name.
func resumeCandidates(m model.Machine, nodes []model.Node, topologies map[string]model.NodeTopology) ([]model.Node, []string) {
	h := m.Status.Hibernation
	var out []model.Node
	var why []string
	for _, n := range nodes {
		name := n.Metadata.Name
		if name == m.Spec.NodeName {
			continue
		}
		facts := topologies[name].Status.Hibernation
		if facts == nil {
			why = append(why, name+": no hibernation facts published")
			continue
		}
		if r := model.HibernationIncompatibilities(h.Host, *facts); len(r) > 0 {
			why = append(why, name+": "+strings.Join(r, "; "))
			continue
		}
		out = append(out, n)
	}
	sort.Strings(why)
	return out, why
}

// reconcileHibernatedResumes picks the node that resumes each hibernated
// Machine set back to Running: its own node when that node is ready and
// has room, otherwise a ready node with room whose FluxVM can load the
// saved state, if the root disk is on shared storage. The chosen node is
// written to status.hibernation.resumeNode; only that node's agent
// resumes, so two nodes never load the same state.
func (c *Controller) reconcileHibernatedResumes(ctx context.Context, machines []model.Machine, nodes []model.Node, nodeLoad map[string]scheduler.NodeLoad, topologies map[string]model.NodeTopology) {
	for _, m := range machines {
		if !awaitingResume(m) {
			continue
		}
		others := withoutMachine(machines, m)
		load := scheduler.BuildNodeLoad(others)
		probe := m
		probe.Spec.NodeName = ""
		topo := scheduler.TopologyInput{Nodes: topologies}
		origin := m.Spec.NodeName
		var originNode []model.Node
		for _, n := range nodes {
			if n.Metadata.Name == origin {
				originNode = append(originNode, n)
			}
		}
		target, originErr := c.Scheduler.ChooseTopology(probe, originNode, others, load, topo)
		if originErr == nil && len(originNode) == 0 {
			originErr = fmt.Errorf("node %s is gone", origin)
		}
		message := ""
		if originErr != nil {
			target = ""
			switch {
			case !m.Status.Hibernation.SharedStorage:
				message = fmt.Sprintf("node %s cannot take it back (%v) and the saved state is node-local; waiting for %s", origin, originErr, origin)
			default:
				candidates, why := resumeCandidates(m, nodes, topologies)
				if len(candidates) > 0 {
					if node, err := c.Scheduler.ChooseTopology(probe, candidates, others, load, topo); err == nil {
						target = node
					} else {
						why = append(why, err.Error())
					}
				}
				if target == "" {
					message = fmt.Sprintf("node %s cannot take it back (%v) and no compatible node has room", origin, originErr)
					if len(why) > 0 {
						message += ": " + strings.Join(why, "; ")
					}
				}
			}
		}
		h := *m.Status.Hibernation
		h.Message = message
		h.ResumeNode = target
		if target == "" {
			if m.Status.Hibernation.Message != message {
				if err := c.Kube.PatchMachineStatusFields(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"hibernation": h}); err != nil {
					c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
				}
			}
			continue
		}
		if target != origin {
			if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
				"spec":     map[string]any{"nodeName": target},
				"metadata": map[string]any{"labels": map[string]any{model.AssignedNodeLabel: target}},
			}); err != nil {
				c.Log.Error("moving hibernated machine failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", target, "error", err)
				continue
			}
			c.Log.Info("resuming hibernated machine on another node", "namespace", m.Namespace(), "machine", m.Metadata.Name, "from", origin, "to", target, "reason", originErr.Error())
			if evErr := c.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, "ResumeElsewhere",
				fmt.Sprintf("resuming on %s: %v", target, originErr), "Normal"); evErr != nil {
				c.Log.Error("machine event record failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", evErr)
			}
			moved := m
			moved.Spec.NodeName = target
			scheduler.Reserve(nodeLoad, target, moved)
		}
		if err := c.Kube.PatchMachineStatusFields(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"hibernation": h}); err != nil {
			c.Log.Error("machine status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	}
}
