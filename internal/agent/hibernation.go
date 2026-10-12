// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

const hibernationSweepEvery = time.Minute

// ensureHibernated saves a running Machine's guest state through FluxVM
// (RAM, vCPUs, devices), which then stops QEMU and frees the node's CPU and
// memory. FluxVM keeps the record; powerState Running resumes the same
// boot (see resumeGate).
func (a *Agent) ensureHibernated(ctx context.Context, m model.Machine) error {
	rec, err := a.current(ctx, m)
	if err != nil {
		return err
	}
	status := m.Status
	status.NodeName = a.NodeName
	if rec == nil {
		if m.Status.Hibernation != nil {
			return nil
		}
		status.Phase = "Pending"
		status.Message = "cannot hibernate: no existing runtime -- set powerState to Running first, then Hibernated"
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "NoRuntime", Message: status.Message,
		})
		return a.patchMachineStatusIfChanged(ctx, m, status)
	}
	if rec.Hibernation == nil {
		if normalizePhase(rec.Status) == "Stopped" {
			status.Message = "cannot hibernate a powered-off runtime -- set powerState to Running first, then Hibernated"
			status.Conditions = model.SetCondition(status.Conditions, model.Condition{
				Type: "Ready", Status: "False", Reason: "HibernateRefused", Message: status.Message,
			})
			return a.patchMachineStatusIfChanged(ctx, m, status)
		}
		started := time.Now()
		hibernated, err := a.Flux.Hibernate(ctx, rec.ID())
		if err != nil {
			status.Message = "hibernate failed: " + err.Error()
			status.Conditions = model.SetCondition(status.Conditions, model.Condition{
				Type: "Hibernated", Status: "False", Reason: "HibernateFailed", Message: status.Message,
			})
			return a.patchMachineStatusIfChanged(ctx, m, status)
		}
		rec = hibernated
		a.Log.Info("hibernated runtime", "namespace", m.Namespace(), "machine", m.Metadata.Name, "runtimeID", rec.ID(), "seconds", time.Since(started).Seconds())
		if rec.Hibernation == nil {
			return fmt.Errorf("FluxVM hibernated %s but returned no hibernation record", rec.ID())
		}
	}
	// The guest is gone from the network until it resumes, same as Halted.
	if err := a.deregisterServiceFabric(ctx, m); err != nil {
		return fmt.Errorf("deregister service fabric membership: %w", err)
	}
	status.Phase = model.PowerStateHibernated
	status.RuntimeID = rec.ID()
	status.GuestIP = ""
	status.Network = nil
	status.Message = ""
	status.AppliedServiceFabricMemberships = nil
	status.Hibernation = hibernationStatus(a.NodeName, rec)
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Ready", Status: "False", Reason: "Hibernated",
	})
	status.Conditions = model.SetCondition(status.Conditions, model.Condition{
		Type: "Hibernated", Status: "True", Reason: "StateSaved",
	})
	return a.patchMachineStatusIfChanged(ctx, m, status)
}

func hibernationStatus(node string, rec *fluxvm.Record) *model.HibernationStatus {
	h := rec.Hibernation
	at := h.Manifest.HibernatedAt.UTC().Truncate(time.Second)
	return &model.HibernationStatus{
		Node:          node,
		RuntimeID:     rec.ID(),
		StateBytes:    int64(h.Manifest.StateBytes),
		StateDir:      h.Dir,
		HibernatedAt:  &at,
		SharedStorage: h.Manifest.SharedStorage,
		Host:          h.Manifest.Host.Model(),
	}
}

// resumeGate runs before a hibernated Machine wanted Running is started.
// It returns handled=true when this tick must stop here: the controller
// has not picked this node to resume it, the import from another node
// failed, or the saved state was just discarded. Otherwise it returns the
// runtime to Start, importing it first when another node hibernated it.
func (a *Agent) resumeGate(ctx context.Context, m model.Machine, rec *fluxvm.Record) (*fluxvm.Record, bool, error) {
	h := m.Status.Hibernation
	if h == nil || (rec != nil && rec.Hibernation == nil && rec.ID() == h.RuntimeID) {
		return rec, false, nil
	}
	status := m.Status
	status.NodeName = a.NodeName
	if model.AnnotationTrue(m.Metadata, model.AnnotationDiscardHibernation) {
		if rec != nil && rec.Hibernation != nil {
			discarded, err := a.Flux.DiscardHibernation(ctx, rec.ID())
			if err != nil {
				return nil, true, fmt.Errorf("discard hibernation: %w", err)
			}
			rec = discarded
		}
		if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{model.AnnotationDiscardHibernation: nil}},
		}); err != nil {
			return nil, true, err
		}
		status.Hibernation = nil
		if rec == nil {
			status.RuntimeID = ""
		}
		status.Message = "saved state discarded; booting from disk"
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Hibernated", Status: "False", Reason: "Discarded",
		})
		return nil, true, a.patchMachineStatusIfChanged(ctx, m, status)
	}
	if h.ResumeNode != a.NodeName {
		status.Phase = model.PowerStateHibernated
		status.Message = "waiting for kairon-controller to pick the node that resumes it"
		if h.Message != "" {
			status.Message = h.Message
		}
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "ResumeWaiting", Message: status.Message,
		})
		return nil, true, a.patchMachineStatusIfChanged(ctx, m, status)
	}
	if rec != nil {
		return rec, false, nil
	}
	imported, err := a.Flux.ImportHibernated(ctx, h.RuntimeID)
	if err != nil {
		status.Phase = "Blocked"
		status.Message = fmt.Sprintf("cannot import the state %s hibernated: %v; annotate %s=true to boot from disk instead", h.Node, err, model.AnnotationDiscardHibernation)
		status.Conditions = model.SetCondition(status.Conditions, model.Condition{
			Type: "Ready", Status: "False", Reason: "ResumeBlocked", Message: status.Message,
		})
		return nil, true, a.patchMachineStatusIfChanged(ctx, m, status)
	}
	a.Log.Info("imported hibernated runtime", "namespace", m.Namespace(), "machine", m.Metadata.Name, "runtimeID", imported.ID(), "from", h.Node)
	return imported, false, nil
}

// sweepMovedHibernations deletes this node's hibernated FluxVM records that
// no Machine here still owns: the Machine was deleted, or it resumed (or
// booted fresh) on another node. FluxVM keeps a state directory another
// node imported.
func (a *Agent) sweepMovedHibernations(ctx context.Context) {
	if a.Kube == nil || a.Flux == nil {
		return
	}
	if a.hibernationSweptAt.IsZero() {
		a.hibernationSweptAt = time.Now()
		return
	}
	if time.Since(a.hibernationSweptAt) < hibernationSweepEvery {
		return
	}
	a.hibernationSweptAt = time.Now()
	recs, err := a.Flux.ListVMs(ctx)
	if err != nil {
		a.log().Debug("FluxVM VM list failed", "error", err)
		return
	}
	var hibernated []fluxvm.Record
	for _, r := range recs {
		if r.Hibernation != nil && strings.HasPrefix(r.Name, "kairon-") {
			hibernated = append(hibernated, r)
		}
	}
	if len(hibernated) == 0 {
		return
	}
	machines, err := a.Kube.ListMachines(ctx)
	if err != nil {
		a.log().Warn("machine list for hibernation sweep failed", "error", err)
		return
	}
	byRuntime := make(map[string]model.Machine, len(machines))
	for _, m := range machines {
		byRuntime[m.RuntimeName()] = m
	}
	for _, r := range hibernated {
		m, ok := byRuntime[r.Name]
		if ok && !hibernationMovedAway(m, a.NodeName, r.ID()) {
			continue
		}
		if err := a.Flux.Delete(ctx, r.ID()); err != nil {
			a.log().Warn("deleting moved hibernated runtime failed", "runtimeID", r.ID(), "error", err)
			continue
		}
		a.log().Info("deleted hibernated runtime no Machine here owns", "runtimeID", r.ID(), "name", r.Name)
	}
}

// hibernationMovedAway reports whether m, though it still exists, no longer
// needs runtime id's hibernated record on node.
func hibernationMovedAway(m model.Machine, node, id string) bool {
	if m.Spec.NodeName == node || m.Spec.NodeName == "" {
		return false
	}
	h := m.Status.Hibernation
	return h == nil || h.RuntimeID != id
}
