// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

// listAgentSessions returns every AgentSession, or none while the CRD is
// not installed yet.
func (c *Controller) listAgentSessions(ctx context.Context) ([]model.AgentSession, error) {
	sessions, err := c.Kube.ListAgentSessions(ctx)
	if kube.IsNotFound(err) {
		return nil, nil
	}
	return sessions, err
}

// reserveAgentSessions adds every session that holds a VM to load, so
// Machines scheduled this tick see the capacity sessions already use.
func reserveAgentSessions(load map[string]scheduler.NodeLoad, sessions []model.AgentSession) {
	for _, s := range sessions {
		if s.Active() {
			scheduler.Reserve(load, s.Spec.NodeName, model.AgentSessionMachine(s))
		}
	}
}

// seedQuotaWithAgentSessions counts every session that holds a VM against
// its namespace's MachineQuotas, as one Machine of its size.
func seedQuotaWithAgentSessions(trackers map[string][]*QuotaTracker, sessions []model.AgentSession) {
	for _, s := range sessions {
		if !s.Active() {
			continue
		}
		cpu, mem := MachineFootprint(model.AgentSessionMachine(s))
		for _, t := range trackers[s.Namespace()] {
			t.used.UsedMachines++
			t.used.UsedTotalCPUCores += cpu
			t.used.UsedTotalMemoryMiB += mem
		}
	}
}

// reconcileAgentSessions places unscheduled sessions on a node that
// advertises model.LabelAgentSessionsCapable, through the same scheduler
// and MachineQuota admission a Machine of the session's size gets, and
// adds the finalizer kairon-node removes once FluxVM has dropped the
// session. A session deleted before it was placed never reached a node, so
// its finalizer is removed here.
func (c *Controller) reconcileAgentSessions(ctx context.Context, sessions []model.AgentSession, nodes []model.Node, machines []model.Machine, load map[string]scheduler.NodeLoad, trackers map[string][]*QuotaTracker) {
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].Metadata.CreationTimestamp.Before(sessions[j].Metadata.CreationTimestamp)
	})
	for _, s := range sessions {
		if s.Metadata.DeletionTimestamp != nil {
			if s.Spec.NodeName == "" && model.HasFinalizerList(s.Metadata.Finalizers, model.FinalizerAgentSession) {
				if err := c.Kube.PatchAgentSession(ctx, s.Namespace(), s.Metadata.Name, map[string]any{"metadata": map[string]any{
					"finalizers": model.RemoveFinalizer(s.Metadata.Finalizers, model.FinalizerAgentSession),
				}}); err != nil && !kube.IsNotFound(err) {
					c.Log.Error("agentsession finalizer removal failed", "namespace", s.Namespace(), "session", s.Metadata.Name, "error", err)
				}
			}
			continue
		}
		if s.Spec.NodeName != "" {
			continue
		}
		m := model.AgentSessionMachine(s)
		node, err := c.Scheduler.Choose(m, nodes, machines, load, "")
		if err != nil {
			c.agentSessionPending(ctx, s, err.Error())
			continue
		}
		if blocker := admitQuota(trackers, m); blocker != "" {
			c.agentSessionPending(ctx, s, blocker)
			continue
		}
		finalizers := s.Metadata.Finalizers
		if !model.HasFinalizerList(finalizers, model.FinalizerAgentSession) {
			finalizers = append(append([]string{}, finalizers...), model.FinalizerAgentSession)
		}
		if err := c.Kube.PatchAgentSession(ctx, s.Namespace(), s.Metadata.Name, map[string]any{
			"spec": map[string]any{"nodeName": node},
			"metadata": map[string]any{
				"labels":     map[string]any{model.AssignedNodeLabel: node},
				"finalizers": finalizers,
			},
		}); err != nil {
			c.Log.Error("agentsession schedule patch failed", "namespace", s.Namespace(), "session", s.Metadata.Name, "error", err)
			continue
		}
		scheduler.Reserve(load, node, m)
		c.Log.Info("scheduled agent session", "namespace", s.Namespace(), "session", s.Metadata.Name, "node", node)
	}
}

func (c *Controller) agentSessionPending(ctx context.Context, s model.AgentSession, message string) {
	if s.Status.Phase == model.AgentSessionPending && s.Status.Message == message {
		return
	}
	if err := c.Kube.PatchAgentSessionStatusFields(ctx, s.Namespace(), s.Metadata.Name, map[string]any{
		"phase": model.AgentSessionPending, "message": message,
	}); err != nil {
		c.Log.Error("agentsession status patch failed", "namespace", s.Namespace(), "session", s.Metadata.Name, "error", err)
	}
}
