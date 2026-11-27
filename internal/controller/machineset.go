// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// defaultMachineSetMaxUnavailable is MachineSetSpec.MaxUnavailable's
// fallback when unset -- matches Kubernetes' own Deployment default.
const defaultMachineSetMaxUnavailable = "1"

// MachineSet Event reasons posted through recordMachineSetEvent.
// ScaleDownBlocked (Warning, transition only) lives in machineset_mdb.go.
const (
	machineSetReasonScaledUp   = "ScaledUp"
	machineSetReasonScaledDown = "ScaledDown"
	machineSetReasonPaused     = "Paused"
	machineSetReasonResumed    = "Resumed"
)

// reconcileMachineSets creates/deletes Machine objects to bring every
// MachineSet's owned replicas in line with its spec -- see
// reconcileMachineSet for the per-object logic -- and cascades a
// MachineSet's own deletion to every Machine it owns (see
// reconcileMachineSetDeletion) rather than leaving them behind, orphaned.
// Uses the machines this tick's Reconcile already listed; no separate API
// call for them.
//
// observed carries each MachineSet with the fresh replicas/readyReplicas/
// updatedReplicas tally reconcileMachineSet just computed from this same
// machines snapshot -- even on a create/delete error, since that tally
// reflects real, already-listed Machine state independent of whether the
// attempted mutation itself succeeded (the same "the count is real even if
// the write wasn't" reasoning reconcileDisruptionBudgetsStatus's own
// observed slice already uses). Handed to Metrics.ObserveMachineSets once
// per tick, the same "Status already computed, just also hand it to
// metrics" shape controller.go's observedQuotas/ObserveQuotas and
// disruption.go's observed/ObserveDisruptionBudgets already established --
// see internal/metrics.Recorder.ObserveMachineSets.
func (c *Controller) reconcileMachineSets(ctx context.Context, machineSets []model.MachineSet, machines []model.Machine) {
	observed := make([]model.MachineSet, 0, len(machineSets))
	// One MachineDisruptionBudget snapshot per tick for scale-down gating
	// (machineset_mdb.go); listed lazily, only if a scale-down needs it.
	ctx = withMachineSetBudgetCache(ctx, machines)
	for _, ms := range machineSets {
		if ms.Metadata.DeletionTimestamp != nil {
			c.reconcileMachineSetDeletion(ctx, ms, machines)
			continue
		}
		status, err := c.reconcileMachineSet(ctx, ms, machines)
		ms.Status = status
		observed = append(observed, ms)
		if err != nil {
			c.Log.Error("machineset reconcile failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", err)
			status.Message = err.Error()
			if statusErr := c.Kube.PatchMachineSetStatus(ctx, ms.Namespace(), ms.Metadata.Name, status); statusErr != nil {
				c.Log.Error("machineset status patch failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", statusErr)
			}
		}
	}
	if c.Metrics != nil {
		c.Metrics.ObserveMachineSets(observed)
		// Membership join key for per-MachineSet usage queries (HPA/KEDA
		// autoscaling, docs/guides/machineset-autoscaling.md).
		c.Metrics.ObserveMachineSetMachines(observed, machines)
	}
}

// machineSetTemplateHash fingerprints spec.template so reconcileMachineSet
// can tell a Machine created from the current template apart from one
// created before the last template change -- the same "revision marker on
// each owned object" idea a real Kubernetes ReplicaSet's own
// pod-template-hash label captures, computed here instead of trusted from
// an external controller since this project has none to delegate to.
//
// The implementation lives in model.MachineSetTemplateHash so kaironctl
// (machineset history/rollback) computes exactly the same value.
func machineSetTemplateHash(t model.MachineTemplate) string {
	return model.MachineSetTemplateHash(t)
}

// machineSetTemplateRevisionPatch returns the metadata merge patch that
// keeps ms's template revision annotations (model.
// AnnotationMachineSetCurrentTemplateHash and friends) in step with its live
// spec.template, or ok=false when they already are. hash is
// machineSetTemplateHash(ms.Spec.Template).
//
//   - First sighting (no current-template-hash yet): record the live
//     template as "current"; there is no "previous" to keep.
//   - Template changed (recorded hash != live hash): the recorded "current"
//     becomes "previous" and the live template becomes "current". This is
//     the only point the old template is still known, which is why
//     "current" has to be recorded at all.
//   - kaironctl machineset rollback writes the swapped annotations in the
//     same PATCH as the restored template, so the controller finds them
//     already in step and leaves them alone -- a second rollback goes
//     forward again.
//
// A template whose JSON exceeds model.MaxMachineSetTemplateAnnotationBytes
// is recorded by hash only (its body annotation is removed): rolling back
// to it is refused by kaironctl instead of storing an object that would
// push the MachineSet past the API server's annotation size limit.
func machineSetTemplateRevisionPatch(ms model.MachineSet, hash string) (map[string]any, bool) {
	ann := ms.Metadata.Annotations
	recorded := ann[model.AnnotationMachineSetCurrentTemplateHash]
	if recorded == hash {
		return nil, false
	}
	var body any // nil deletes the annotation under JSON merge patch
	if b, err := json.Marshal(ms.Spec.Template); err == nil && len(b) <= model.MaxMachineSetTemplateAnnotationBytes {
		body = string(b)
	}
	patch := map[string]any{
		model.AnnotationMachineSetCurrentTemplateHash: hash,
		model.AnnotationMachineSetCurrentTemplate:     body,
	}
	if recorded != "" {
		patch[model.AnnotationMachineSetPreviousTemplateHash] = recorded
		var prev any
		if v, ok := ann[model.AnnotationMachineSetCurrentTemplate]; ok && v != "" {
			prev = v
		}
		patch[model.AnnotationMachineSetPreviousTemplate] = prev
	}
	return map[string]any{"metadata": map[string]any{"annotations": patch}}, true
}

// recordMachineSetTemplateRevision applies machineSetTemplateRevisionPatch.
// A failure is logged, not returned: it is bookkeeping for rollback only and
// must never hold up a rollout or scale. Nothing is lost by failing -- the
// recorded "current" is unchanged, so the next tick computes the same
// move again.
func (c *Controller) recordMachineSetTemplateRevision(ctx context.Context, ms model.MachineSet, hash string) {
	patch, ok := machineSetTemplateRevisionPatch(ms, hash)
	if !ok {
		return
	}
	if err := c.Kube.PatchMachineSet(ctx, ms.Namespace(), ms.Metadata.Name, patch); err != nil {
		c.Log.Error("machineset template revision annotation patch failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", err)
		return
	}
	if prev := ms.Metadata.Annotations[model.AnnotationMachineSetCurrentTemplateHash]; prev != "" {
		c.Log.Info("machineset template changed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "previousTemplateHash", prev, "templateHash", hash)
	}
}

// machineSetPausedCondition is the status.conditions entry mirroring
// spec.paused. Its Message is fixed per state so model.SetCondition keeps
// LastTransitionTime at the moment pause actually flipped.
func machineSetPausedCondition(paused bool) model.Condition {
	if paused {
		return model.Condition{Type: "Paused", Status: "True", Reason: "Paused", Message: "spec.paused is true; no replicas are created or deleted"}
	}
	return model.Condition{Type: "Paused", Status: "False", Reason: "NotPaused"}
}

// recordMachineSetEvent posts a core/v1 Event whose involvedObject is ms.
// Failures are logged, not returned: an Event is operator signal, never a
// reason to stall a scale or rollout. Uses kube.Client.RecordEvent, the
// same path Machine quota, health and DRS events already take.
func (c *Controller) recordMachineSetEvent(ctx context.Context, ms model.MachineSet, reason, message, eventType string) {
	if err := c.Kube.RecordEvent(ctx, ms.Namespace(), model.KindMachineSet, ms.Metadata.Name, ms.Metadata.UID, reason, message, eventType); err != nil {
		c.Log.Error("machineset event failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", err)
	}
}

// machineSetPauseEvents records a Normal Paused or Resumed Event when
// spec.paused flips relative to the previous status.paused. Steady ticks
// (already paused, or already running) emit nothing, so Event history
// stays quiet while an operator leaves a set paused.
func (c *Controller) machineSetPauseEvents(ctx context.Context, ms model.MachineSet) {
	if ms.Spec.Paused == ms.Status.Paused {
		return
	}
	if ms.Spec.Paused {
		c.recordMachineSetEvent(ctx, ms, machineSetReasonPaused, "spec.paused is true; no replicas are created or deleted", "Normal")
		return
	}
	c.recordMachineSetEvent(ctx, ms, machineSetReasonResumed, "spec.paused is false; reconciliation resumed", "Normal")
}

// ownedMachines returns machines carrying ms's LabelMachineSet label,
// excluding any already being deleted (deletion is already in flight;
// counting it toward "current" would double-book capacity that's about
// to disappear on its own).
func ownedMachines(ms model.MachineSet, machines []model.Machine) []model.Machine {
	var owned []model.Machine
	for _, m := range machines {
		if m.Namespace() == ms.Namespace() && m.Metadata.Labels[model.LabelMachineSet] == ms.Metadata.Name && m.Metadata.DeletionTimestamp == nil {
			owned = append(owned, m)
		}
	}
	return owned
}

// reconcileMachineSetDeletion cascades a MachineSet's own deletion to every
// Machine it owns. This project has no ownerReference/garbage-collection
// mechanism to do this for free (see LabelMachineSet's own doc comment) --
// without this, deleting a MachineSet would simply remove that one
// Kubernetes object while every replica it created (each one a real,
// independently-running FluxVM VM, per MachineSet's own doc comment) kept
// running underneath it, orphaned: no MachineSet left to scale, roll out,
// or account for them, yet each replica's own resources (quota usage,
// attached volumes, network policy, ...) keep being consumed indefinitely.
//
// Fails closed exactly like every other finalizer-guarded delete in this
// project (reconcileSecurityGroup, reconcileMachineNetworkPolicy,
// Agent.cleanup): FinalizerMachineSet is only removed once every Machine
// still carrying this MachineSet's LabelMachineSet label is actually gone,
// not merely deletion-requested -- a Machine's own runtime-cleanup
// finalizer (internal/agent) can keep it around for several more ticks
// after DeleteMachine is called, and a MachineSet whose finalizer cleared
// early would vanish from Kubernetes while that Machine (and its FluxVM VM)
// was still very much alive. A DeleteMachine failure on one owned Machine
// leaves the finalizer in place and returns without touching the rest this
// tick -- retried whole, from the same live listing, next tick -- rather
// than silently skip over the one that failed.
func (c *Controller) reconcileMachineSetDeletion(ctx context.Context, ms model.MachineSet, machines []model.Machine) {
	if !model.HasFinalizerList(ms.Metadata.Finalizers, model.FinalizerMachineSet) {
		// Never carried the finalizer (created before this existed, or the
		// initial finalizer-add patch never landed before delete) -- nothing
		// to fail closed on, exactly like reconcileMachineNetworkPolicy's own
		// deletion path handles the equivalent case.
		return
	}
	var owned []model.Machine
	for _, m := range machines {
		if m.Namespace() == ms.Namespace() && m.Metadata.Labels[model.LabelMachineSet] == ms.Metadata.Name {
			owned = append(owned, m)
		}
	}
	for _, m := range owned {
		if m.Metadata.DeletionTimestamp != nil {
			continue // already being torn down; nothing more to do for it this tick
		}
		if err := c.Kube.DeleteMachine(ctx, m.Namespace(), m.Metadata.Name); err != nil {
			c.Log.Error("machineset deletion: delete owned machine failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "machine", m.Metadata.Name, "error", err)
			return
		}
	}
	if len(owned) > 0 {
		// At least one owned Machine still exists (even if every DeleteMachine
		// call above just succeeded) -- its own finalizer means it isn't gone
		// from Kubernetes yet. Leave FinalizerMachineSet in place and let a
		// later tick's fresh Machine listing notice once it truly is.
		return
	}
	finals := model.RemoveFinalizer(ms.Metadata.Finalizers, model.FinalizerMachineSet)
	if err := c.Kube.PatchMachineSet(ctx, ms.Namespace(), ms.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
		c.Log.Error("machineset deletion: finalizer removal failed", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "error", err)
	}
}

// reconcileMachineSet advances one MachineSet by exactly one step per
// tick -- never the whole rollout in one pass, so a rollout is always
// paced by (and visible across) real reconcile intervals, the same
// incremental-progress shape every other reconcile loop in this project
// already has. A RollingUpdate's own pace is gated by how many current-
// template replicas are actually Ready (see stepMachineSetToward's own
// comment) -- not just by how many exist, closing what used to be a real
// gap documented here (a freshly created replica still becoming Ready
// used to count as available capacity, letting the rollout disrupt an
// old, healthy replica before its replacement was confirmed healthy).
// Real limit still true, not silently assumed away: this paces purely by
// each tick's own snapshot of Phase == "Running", not a live watch or any
// deeper application-level readiness signal (e.g. a guest agent heartbeat)
// -- see docs/guides/machine-sets.md.
//
// Returns the status it computed regardless of outcome -- on success,
// exactly what it also just patched; on a create/delete error from
// stepMachineSetToward, the tally computed from this tick's Machine
// snapshot before that step ever ran, still accurate independent of
// whether the attempted mutation itself succeeded (the caller,
// reconcileMachineSets, uses it for both the error-message status patch
// and Metrics.ObserveMachineSets).
func (c *Controller) reconcileMachineSet(ctx context.Context, ms model.MachineSet, machines []model.Machine) (model.MachineSetStatus, error) {
	desired := ms.Spec.Replicas
	if desired < 0 {
		desired = 0
	}
	strategy := ms.Spec.Strategy
	if strategy == "" {
		strategy = "RollingUpdate"
	}
	hash := machineSetTemplateHash(ms.Spec.Template)
	// Record the template revision annotations (current/previous hash and
	// a copy of each template) for `kaironctl machineset history` and
	// `rollback`. Done even while paused: it creates and deletes nothing.
	c.recordMachineSetTemplateRevision(ctx, ms, hash)
	// Compare spec.paused to the previous status.paused before this tick
	// overwrites it, so a flip emits exactly one Event.
	c.machineSetPauseEvents(ctx, ms)
	owned := ownedMachines(ms, machines)

	var current, outdated []model.Machine
	readyCurrent := 0
	for _, m := range owned {
		if m.Metadata.Labels[model.LabelMachineSetTemplateHash] == hash {
			current = append(current, m)
			if m.Status.Phase == "Running" {
				readyCurrent++
			}
		} else {
			outdated = append(outdated, m)
		}
	}

	status := model.MachineSetStatus{
		ObservedGeneration: ms.Metadata.Generation,
		Replicas:           len(owned),
		ReadyReplicas:      readyCurrent,
		UpdatedReplicas:    len(current),
		Selector:           model.LabelMachineSet + "=" + ms.Metadata.Name,
		Paused:             ms.Spec.Paused,
		// Carried forward so a failed step never wipes existing conditions.
		// Paused goes through model.SetCondition so its lastTransitionTime
		// marks when pause actually flipped.
		Conditions: model.SetCondition(ms.Status.Conditions, machineSetPausedCondition(ms.Spec.Paused)),
	}

	// Added once, before any owned Machine is ever created -- guarantees
	// reconcileMachineSetDeletion's own cascade always has a finalizer to
	// fail closed on by the time a delete could arrive, the same
	// finalizer-before-first-mutation ordering reconcileSecurityGroup already
	// uses.
	if !model.HasFinalizerList(ms.Metadata.Finalizers, model.FinalizerMachineSet) {
		finals := append(append([]string{}, ms.Metadata.Finalizers...), model.FinalizerMachineSet)
		if err := c.Kube.PatchMachineSet(ctx, ms.Namespace(), ms.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
			return status, fmt.Errorf("add machineset finalizer: %w", err)
		}
	}

	err := c.stepMachineSetToward(ctx, ms, hash, strategy, desired, current, outdated, readyCurrent)
	var blocked *scaleDownBlockedError
	if errors.As(err, &blocked) {
		err = nil // a budget hold-back is reported as a condition, not a failure
	}
	if err != nil {
		return status, err
	}
	// Progressing: True while any owned Machine is still on an older template.
	progressing := len(outdated) > 0 || len(current) < desired
	reason := "Complete"
	msg := "all replicas are on the current template"
	if progressing {
		reason = "Updating"
		msg = fmt.Sprintf("%d of %d replicas updated", len(current), desired)
	}
	status.Conditions = upsertCondition(status.Conditions, model.Condition{
		Type:    "Progressing",
		Status:  boolStr(progressing),
		Reason:  reason,
		Message: msg,
	})
	// Available: True when every desired replica is Running.
	available := readyCurrent >= desired && desired > 0
	availReason := "MinimumReplicasAvailable"
	availMsg := fmt.Sprintf("%d ready", readyCurrent)
	if !available {
		availReason = "MinimumReplicasUnavailable"
		availMsg = fmt.Sprintf("%d of %d ready", readyCurrent, desired)
	}
	status.Conditions = upsertCondition(status.Conditions, model.Condition{
		Type:    "Available",
		Status:  boolStr(available),
		Reason:  availReason,
		Message: availMsg,
	})
	// ReplicaFailure: True if any owned Machine is Phase=Failed.
	failed := 0
	for _, m := range owned {
		if m.Status.Phase == "Failed" {
			failed++
		}
	}
	status.Conditions = upsertCondition(status.Conditions, model.Condition{
		Type:    "ReplicaFailure",
		Status:  boolStr(failed > 0),
		Reason:  "FailedReplicas",
		Message: fmt.Sprintf("%d failed", failed),
	})
	status.Conditions = c.machineSetScaleDownCondition(ctx, ms, status.Conditions, blocked)
	return status, c.Kube.PatchMachineSetStatus(ctx, ms.Namespace(), ms.Metadata.Name, status)
}

// stepMachineSetToward performs at most one create-or-delete batch per
// call -- see reconcileMachineSet's own doc comment for why a whole
// rollout is deliberately never collapsed into a single tick. readyCurrent
// is how many of current are Phase == "Running" -- used only by the
// RollingUpdate branch below to gate further disruption on actual
// readiness, not mere existence (a Machine object can exist immediately
// after CreateMachine returns, long before kairon-node has even scheduled
// it, let alone booted it to Running).
//
// A paused MachineSet (spec.paused) takes no step at all: no scale-up,
// scale-down or rollout replacement. reconcileMachineSet still patches
// status, so replica counts and the Paused condition stay current while
// frozen, and the next unpaused tick resumes toward whatever spec says
// then.
func (c *Controller) stepMachineSetToward(ctx context.Context, ms model.MachineSet, hash, strategy string, desired int, current, outdated []model.Machine, readyCurrent int) error {
	if ms.Spec.Paused {
		return nil // freeze: no creates or deletes while paused
	}
	total := len(current) + len(outdated)

	// Under-provisioned (net-new scale-up, or a replica died on its own)
	// always takes priority over any rollout concern: fill the gap with
	// current-template replicas regardless of strategy.
	if total < desired {
		return c.createMachineSetReplicas(ctx, ms, hash, desired-total, total)
	}

	if strategy == "Recreate" {
		// Terminate every outdated replica before creating any
		// replacement -- matches a real Kubernetes Deployment's own
		// Recreate strategy (full stop, then restart), the simplest
		// correct behavior and the one requiring no maxUnavailable
		// bookkeeping at all.
		if len(outdated) > 0 {
			return c.deleteMachines(ctx, ms, outdated, total)
		}
		if len(current) > desired {
			return c.scaleDownMachineSet(ctx, ms, current, len(current)-desired)
		}
		return nil
	}

	// RollingUpdate: replace outdated replicas while respecting both
	// maxUnavailable and maxSurge. With maxSurge > 0 we may create new
	// (current-template) replicas before deleting old ones, up to
	// desired+maxSurge total. Without surge the previous behavior is
	// preserved: only delete when it would not drop available below
	// desired-maxUnavailable, then create on a later tick once total
	// falls below desired.
	if len(outdated) > 0 {
		maxUnavailable, err := resolveMaxUnavailable(ms.Spec.MaxUnavailable, desired)
		if err != nil {
			return fmt.Errorf("spec.maxUnavailable: %w", err)
		}
		maxSurge, err := resolveMaxSurge(ms.Spec.MaxSurge, desired)
		if err != nil {
			return fmt.Errorf("spec.maxSurge: %w", err)
		}
		// Prefer creating new replicas first when surge budget remains.
		if maxSurge > 0 && total < desired+maxSurge {
			toCreate := (desired + maxSurge) - total
			if toCreate > maxSurge {
				toCreate = maxSurge
			}
			return c.createMachineSetReplicas(ctx, ms, hash, toCreate, total)
		}
		minAvailable := desired - maxUnavailable
		available := readyCurrent + len(outdated)
		canDelete := available - minAvailable
		if canDelete <= 0 {
			return nil
		}
		if canDelete > len(outdated) {
			canDelete = len(outdated)
		}
		return c.deleteMachines(ctx, ms, outdated[:canDelete], total)
	}
	if len(current) > desired {
		// Pure scale-down (HPA/KEDA/kubectl scale lowered spec.replicas):
		// gated by MachineDisruptionBudgets -- see machineset_mdb.go.
		return c.scaleDownMachineSet(ctx, ms, current, len(current)-desired)
	}
	return nil
}

func resolveMaxSurge(spec string, desired int) (int, error) {
	if spec == "" || spec == "0" || spec == "0%" {
		return 0, nil
	}
	n, err := model.ParseIntOrPercent(spec, desired)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

func resolveMaxUnavailable(spec string, desired int) (int, error) {
	if spec == "" {
		spec = defaultMachineSetMaxUnavailable
	}
	n, err := model.ParseIntOrPercent(spec, desired)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		n = 1 // matches Kubernetes' own Deployment: a 0 rounds up to make progress possible at all
	}
	return n, nil
}

func (c *Controller) deleteMachines(ctx context.Context, ms model.MachineSet, machines []model.Machine, fromReplicas int) error {
	replicas := fromReplicas
	for _, m := range machines {
		if err := c.Kube.DeleteMachine(ctx, m.Namespace(), m.Metadata.Name); err != nil {
			return fmt.Errorf("delete machine %s/%s: %w", m.Namespace(), m.Metadata.Name, err)
		}
		replicas--
		c.recordMachineSetEvent(ctx, ms, machineSetReasonScaledDown,
			fmt.Sprintf("deleted machine %s, replicas %d->%d", m.Metadata.Name, replicas+1, replicas), "Normal")
		c.Log.Info("machineset deleted replica", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "machine", m.Metadata.Name)
	}
	return nil
}

func (c *Controller) createMachineSetReplicas(ctx context.Context, ms model.MachineSet, hash string, n, fromReplicas int) error {
	replicas := fromReplicas
	for i := 0; i < n; i++ {
		labels := map[string]string{}
		for k, v := range ms.Spec.Template.Labels {
			labels[k] = v
		}
		labels[model.LabelMachineSet] = ms.Metadata.Name
		labels[model.LabelMachineSetTemplateHash] = hash
		machine := model.Machine{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
			Metadata: model.ObjectMeta{Name: generateMachineSetChildName(ms.Metadata.Name), Namespace: ms.Namespace(), Labels: labels},
			Spec:     ms.Spec.Template.Spec,
		}
		if _, err := c.Kube.CreateMachine(ctx, ms.Namespace(), machine); err != nil {
			return fmt.Errorf("create machine for machineset %s/%s: %w", ms.Namespace(), ms.Metadata.Name, err)
		}
		replicas++
		c.recordMachineSetEvent(ctx, ms, machineSetReasonScaledUp,
			fmt.Sprintf("created machine %s, replicas %d->%d", machine.Metadata.Name, replicas-1, replicas), "Normal")
		c.Log.Info("machineset created replica", "namespace", ms.Namespace(), "machineset", ms.Metadata.Name, "machine", machine.Metadata.Name)
	}
	return nil
}

// generateMachineSetChildName mirrors a real ReplicaSet's own
// <name>-<random suffix> convention -- random rather than sequential so
// two controller replicas racing to create a replica (which should never
// happen given leader election, but costs nothing to make safe anyway)
// can't collide on the same generated name.
func generateMachineSetChildName(machineSetName string) string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return machineSetName + "-" + hex.EncodeToString(buf)
}

func boolStr(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func upsertCondition(conds []model.Condition, c model.Condition) []model.Condition {
	for i, existing := range conds {
		if existing.Type == c.Type {
			conds[i] = c
			return conds
		}
	}
	return append(conds, c)
}
