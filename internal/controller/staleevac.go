// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/zyvorai/kairon/internal/fencing"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/nodeliveness"
)

// DefaultStaleEvacuationMaxPerTick is StaleEvacuation.MaxPerTick's
// fallback when unset.
const DefaultStaleEvacuationMaxPerTick = 10

// StaleEvacuation makes kairon-controller do what `kaironctl fence` does,
// automatically, for opted-in Machines on a node an operator (or an
// out-of-band power-fencing tool) has attested is dead. It never starts a
// migration: the VM is gone with its node, so the Machine is simply
// handed back to the scheduler. Every gate must hold:
//
//   - the controller runs with Enabled (-stale-evacuation);
//   - the Node exists and carries fencing.AnnotationNodeFenced;
//   - the Node is not Ready, or (with NodeLivenessLeaseNamespace set) its
//     kairon-node liveness Lease is stale. A fresh Lease, or a lookup
//     error other than NotFound, refuses;
//   - the Machine has model.ConditionNodeUnreachable=True and no
//     non-terminal migration;
//   - the Machine has fencing.AnnotationEvacuate=true;
//   - every MachineDisruptionBudget matching it allows one more disruption.
//
// A Machine without the opt-in is only logged, once per controller run.
// Off by default: an unreachable node alone never reschedules anything.
type StaleEvacuation struct {
	Enabled bool
	// MaxPerTick caps how many Machines are fenced per reconcile tick, so
	// a wrongly annotated node cannot reschedule the fleet in one pass.
	MaxPerTick int
}

func (s StaleEvacuation) maxPerTick() int {
	if s.MaxPerTick > 0 {
		return s.MaxPerTick
	}
	return DefaultStaleEvacuationMaxPerTick
}

// fencedNodes returns the attested-dead nodes (name -> attestation) that
// pass the node-level gates.
func (c *Controller) fencedNodes(ctx context.Context, nodes []model.Node) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		reason := strings.TrimSpace(n.Metadata.Annotations[fencing.AnnotationNodeFenced])
		if reason == "" {
			continue
		}
		name := n.Metadata.Name
		leaseStale := false
		if c.NodeLivenessLeaseNamespace != "" {
			lease, err := c.Kube.GetLease(ctx, c.NodeLivenessLeaseNamespace, nodeliveness.LeaseName(name))
			switch {
			case kube.IsNotFound(err):
			case err != nil:
				c.Log.Warn("stale evacuation: liveness lease lookup failed; not evacuating this tick", "node", name, "error", err)
				continue
			case nodeliveness.IsFresh(lease, 0):
				c.staleEvacNoteOnce("lease:"+name, "stale evacuation: node is fenced but kairon-node's liveness lease is still fresh; not evacuating", "node", name)
				continue
			default:
				leaseStale = true
			}
		}
		if nodeReady(n) && !leaseStale {
			c.staleEvacNoteOnce("ready:"+name, "stale evacuation: node carries the fenced annotation but is Ready; ignoring it", "node", name)
			continue
		}
		out[name] = reason
	}
	return out
}

func (c *Controller) staleEvacNoteOnce(key, msg string, args ...any) {
	c.staleEvacMu.Lock()
	defer c.staleEvacMu.Unlock()
	if c.staleEvacNoted == nil {
		c.staleEvacNoted = map[string]bool{}
	}
	if c.staleEvacNoted[key] {
		return
	}
	c.staleEvacNoted[key] = true
	c.Log.Warn(msg, args...)
}

func (c *Controller) reconcileStaleEvacuation(ctx context.Context, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) {
	if !c.StaleEvacuation.Enabled {
		return
	}
	dead := c.fencedNodes(ctx, nodes)
	if len(dead) == 0 {
		return
	}
	budgets, err := c.Kube.ListMachineDisruptionBudgets(ctx)
	if err != nil && !kube.IsNotFound(err) {
		c.Log.Error("stale evacuation: list machine disruption budgets failed", "error", err)
		return
	}
	states, err := LoadBudgetStates(budgets, machines, migrations)
	if err != nil {
		c.Log.Error("stale evacuation: load budget states failed", "error", err)
		return
	}
	inFlight := map[string]bool{}
	for _, mig := range migrations {
		if !cordonEvacuateTerminal(mig.Status.Phase) {
			inFlight[mig.Namespace()+"/"+mig.Spec.MachineName] = true
		}
	}
	fenced := 0
	for _, m := range machines {
		node := m.Spec.NodeName
		attestation, ok := dead[node]
		if !ok || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		if cond, found := model.FindCondition(m.Status.Conditions, model.ConditionNodeUnreachable); !found || cond.Status != "True" {
			continue
		}
		key := m.Namespace() + "/" + m.Metadata.Name
		if inFlight[key] {
			continue
		}
		if !model.AnnotationTrue(m.Metadata, fencing.AnnotationEvacuate) {
			c.staleEvacNoteOnce("optout:"+key+"@"+node, "stale evacuation: machine on a fenced node is not opted in (kairon.zyvor.dev/evacuate=true); leaving it for kaironctl fence",
				"namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node)
			continue
		}
		if fenced >= c.StaleEvacuation.maxPerTick() {
			c.Log.Info("stale evacuation: per-tick limit reached; continuing next tick", "limit", c.StaleEvacuation.maxPerTick())
			return
		}
		if blocker := AdmitDisruption(states, m); blocker != "" {
			c.Log.Info("stale evacuation: blocked by disruption budget", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "reason", blocker)
			continue
		}
		msg := fmt.Sprintf("stale-evacuation: node %q attested dead: %s", node, attestation)
		if err := fencing.Fence(ctx, c.Kube, m, "StaleEvacuation", msg); err != nil {
			c.Log.Error("stale evacuation: fence failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "error", err)
			continue
		}
		fenced++
		c.Log.Warn("stale evacuation: fenced machine off dead node; the scheduler will place it again", "namespace", m.Namespace(), "machine", m.Metadata.Name, "node", node, "attestation", attestation)
	}
}
