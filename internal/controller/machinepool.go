// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileMachineClaims binds pending claims to Running warm members and
// handles claim deletion. It returns the names of Machines bound this tick
// (keyed namespace/name) so reconcileMachinePools, working from the same
// Machine snapshot, stops counting them as warm and refills immediately.
func (c *Controller) reconcileMachineClaims(ctx context.Context, claims []model.MachineClaim, machines []model.Machine) map[string]bool {
	bound := map[string]bool{}
	byName := map[string]model.Machine{}
	for _, m := range machines {
		byName[m.Namespace()+"/"+m.Metadata.Name] = m
	}
	// Oldest claim first, so a burst of claims against a short pool is
	// served in arrival order.
	sort.SliceStable(claims, func(i, j int) bool {
		return claims[i].Metadata.CreationTimestamp.Before(claims[j].Metadata.CreationTimestamp)
	})
	for _, claim := range claims {
		if claim.Metadata.DeletionTimestamp != nil {
			c.releaseMachineClaim(ctx, claim, byName)
			continue
		}
		status, err := c.reconcileMachineClaim(ctx, claim, machines, byName, bound)
		if err != nil {
			c.Log.Error("machineclaim reconcile failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "error", err)
			status.Message = err.Error()
		}
		if status == claim.Status {
			continue
		}
		if err := c.Kube.PatchMachineClaimStatus(ctx, claim.Namespace(), claim.Metadata.Name, status); err != nil {
			c.Log.Error("machineclaim status patch failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "error", err)
		}
	}
	return bound
}

func (c *Controller) reconcileMachineClaim(ctx context.Context, claim model.MachineClaim, machines []model.Machine, byName map[string]model.Machine, bound map[string]bool) (model.MachineClaimStatus, error) {
	status := claim.Status
	if status.MachineName != "" {
		m, ok := byName[claim.Namespace()+"/"+status.MachineName]
		if !ok || m.Metadata.DeletionTimestamp != nil {
			status.Phase = model.ClaimLost
			status.Message = fmt.Sprintf("machine %s no longer exists", status.MachineName)
			return status, nil
		}
		status.Phase = model.ClaimBound
		status.Message = ""
		c.accrueClaimUsage(&status, claim, m, time.Now())
		policy, err := c.ensureClaimEgress(ctx, claim, status.MachineName)
		status.EgressPolicy = policy
		if err != nil {
			return status, err
		}
		if claim.Spec.TTLSeconds > 0 && status.BoundAt != nil && time.Since(*status.BoundAt) >= time.Duration(claim.Spec.TTLSeconds)*time.Second {
			if err := c.Kube.DeleteMachineClaim(ctx, claim.Namespace(), claim.Metadata.Name); err != nil && !kube.IsNotFound(err) {
				return status, fmt.Errorf("delete expired claim: %w", err)
			}
			c.Log.Info("machineclaim expired", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "ttlSeconds", claim.Spec.TTLSeconds)
		}
		return status, nil
	}
	if claim.Spec.PoolName == "" {
		status.Phase = model.ClaimPending
		return status, fmt.Errorf("spec.poolName is required")
	}
	if !model.HasFinalizerList(claim.Metadata.Finalizers, model.FinalizerMachineClaim) {
		finals := append(append([]string{}, claim.Metadata.Finalizers...), model.FinalizerMachineClaim)
		if err := c.Kube.PatchMachineClaim(ctx, claim.Namespace(), claim.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
			status.Phase = model.ClaimPending
			return status, fmt.Errorf("add machineclaim finalizer: %w", err)
		}
	}

	candidates, warming := warmMembers(claim.Namespace(), claim.Spec.PoolName, machines, bound)
	filtered := candidates[:0]
	for _, m := range candidates {
		if agentplane.EligibleWarm(claim, m) {
			filtered = append(filtered, m)
		}
	}
	candidates = filtered
	for _, m := range candidates {
		err := c.bindMachine(ctx, claim, m)
		if kube.IsConflict(err) {
			continue // changed since this tick's listing; try the next one
		}
		if err != nil {
			status.Phase = model.ClaimPending
			return status, fmt.Errorf("bind machine %s: %w", m.Metadata.Name, err)
		}
		bound[m.Namespace()+"/"+m.Metadata.Name] = true
		now := time.Now().UTC()
		status.Phase = model.ClaimBound
		status.MachineName = m.Metadata.Name
		status.BoundAt = &now
		if !claim.Metadata.CreationTimestamp.IsZero() {
			status.BindMillis = now.Sub(claim.Metadata.CreationTimestamp).Milliseconds()
		}
		status.Message = ""
		policy, err := c.ensureClaimEgress(ctx, claim, m.Metadata.Name)
		status.EgressPolicy = policy
		if err != nil {
			return status, err
		}
		c.Log.Info("machineclaim bound", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "machine", m.Metadata.Name, "pool", claim.Spec.PoolName)
		return status, nil
	}
	status.Phase = model.ClaimPending
	status.Message = fmt.Sprintf("no Running warm machine in pool %q (%d warming)", claim.Spec.PoolName, warming)
	return status, nil
}

// accrueClaimUsage meters the bound Machine into status.Usage and the
// claim usage metrics.
func (c *Controller) accrueClaimUsage(status *model.MachineClaimStatus, claim model.MachineClaim, m model.Machine, now time.Time) {
	var bound time.Time
	if status.BoundAt != nil {
		bound = *status.BoundAt
	}
	vcpus, memGiB := 0.0, 0.0
	if n, err := model.ParseVCPUs(m.Spec.Resources.CPU); err == nil {
		vcpus = float64(n)
	}
	if mib, err := model.ParseMemoryMiB(m.Spec.Resources.Memory); err == nil {
		memGiB = float64(mib) / 1024
	}
	usage, active, suspended := model.AccrueClaimUsage(status.Usage, bound, m.Status.Phase, vcpus, memGiB, now)
	status.Usage = usage
	if c.Metrics != nil {
		c.Metrics.ObserveClaimUsage(claim.Namespace(), active, suspended, active*vcpus, active*memGiB)
	}
}

// warmMembers returns the pool's Running, unclaimed members, oldest first,
// and how many more are still booting.
func warmMembers(ns, pool string, machines []model.Machine, bound map[string]bool) (ready []model.Machine, warming int) {
	for _, m := range machines {
		if m.Namespace() != ns || m.Metadata.Labels[model.LabelMachinePool] != pool ||
			m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm ||
			m.Metadata.DeletionTimestamp != nil || bound[ns+"/"+m.Metadata.Name] {
			continue
		}
		if warmReady(m) {
			ready = append(ready, m)
		} else {
			warming++
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].Metadata.CreationTimestamp.Before(ready[j].Metadata.CreationTimestamp)
	})
	return ready, warming
}

// warmReady reports whether a warm member can be bound now: Running, or
// Paused by a Paused pool (binding resumes it).
func warmReady(m model.Machine) bool {
	return m.Status.Phase == "Running" || (m.Status.Phase == "Paused" && m.Spec.PowerState == "Paused")
}

// warmPowerTarget is the powerState a warm member should be patched to
// for its pool's warmPowerState, or "" to leave it: a Paused pool pauses
// members once they have run for warmupSeconds; a Running pool resumes
// any its earlier Paused setting left parked.
func warmPowerTarget(spec model.MachinePoolSpec, m model.Machine, now time.Time) string {
	if spec.WarmPowerState == "Paused" {
		if m.Status.Phase == "Running" && (m.Spec.PowerState == "" || m.Spec.PowerState == "Running") && now.Sub(runningSince(m)) >= spec.Warmup() {
			return "Paused"
		}
		return ""
	}
	if m.Spec.PowerState == "Paused" && spec.Template.Spec.PowerState != "Paused" {
		return "Running"
	}
	return ""
}

// runningSince is when m last became Ready, else its creation time.
func runningSince(m model.Machine) time.Time {
	for _, cond := range m.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == "True" && !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime
		}
	}
	return m.Metadata.CreationTimestamp
}

// bindMachine relabels m as claimed, resumes it if its pool paused it, and
// restarts its spec.lifecycle clocks. The patch carries m's
// resourceVersion, so a Machine another writer touched since the listing
// fails with 409 instead of being bound twice.
func (c *Controller) bindMachine(ctx context.Context, claim model.MachineClaim, m model.Machine) error {
	labels := map[string]any{}
	for k, v := range claim.Spec.Labels {
		labels[k] = v
	}
	labels[model.LabelPoolState] = model.PoolStateClaimed
	labels[model.LabelMachineClaim] = claim.Metadata.Name
	now := time.Now().UTC().Format(time.RFC3339)
	meta := map[string]any{"labels": labels, "annotations": map[string]any{
		model.AnnotationLifecycleStart: now,
		model.AnnotationLastActivity:   now,
	}}
	if m.Metadata.ResourceVersion != "" {
		meta["resourceVersion"] = m.Metadata.ResourceVersion
	}
	patch := map[string]any{"metadata": meta}
	if m.Spec.PowerState == "Paused" {
		patch["spec"] = map[string]any{"powerState": "Running"}
	}
	return c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch)
}

// ensureClaimEgress keeps the claim's egress MachineNetworkPolicy in step
// with spec.egress for its bound Machine and returns its name ("" when
// the claim has no allowlist; a previously created one is deleted).
func (c *Controller) ensureClaimEgress(ctx context.Context, claim model.MachineClaim, machine string) (string, error) {
	ns, name := claim.Namespace(), model.ClaimEgressPolicyName(claim.Metadata.Name)
	if claim.Spec.Egress == nil {
		if claim.Status.EgressPolicy != "" {
			if err := c.Kube.DeleteMachineNetworkPolicy(ctx, ns, claim.Status.EgressPolicy); err != nil && !kube.IsNotFound(err) {
				return claim.Status.EgressPolicy, fmt.Errorf("delete egress policy: %w", err)
			}
		}
		return "", nil
	}
	spec := model.MachineNetworkPolicySpec{MachineName: machine, Policy: claim.Spec.Egress.Policy()}
	existing, err := c.Kube.GetMachineNetworkPolicy(ctx, ns, name)
	if kube.IsNotFound(err) {
		_, err = c.Kube.CreateMachineNetworkPolicy(ctx, ns, model.MachineNetworkPolicy{
			TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: "MachineNetworkPolicy"},
			Metadata: model.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{model.LabelMachineClaim: claim.Metadata.Name}},
			Spec:     spec,
		})
		if err != nil {
			return "", fmt.Errorf("create egress policy: %w", err)
		}
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("get egress policy: %w", err)
	}
	if existing.Spec.MachineName != spec.MachineName || !reflect.DeepEqual(existing.Spec.Policy, spec.Policy) {
		patch := map[string]any{"spec": map[string]any{"machineName": spec.MachineName, "selector": nil, "policy": spec.Policy}}
		if err := c.Kube.PatchMachineNetworkPolicy(ctx, ns, name, patch); err != nil {
			return name, fmt.Errorf("update egress policy: %w", err)
		}
	}
	return name, nil
}

// releaseMachineClaim deletes (or, for Retain, unlabels) the claim's
// Machine, then drops the finalizer.
func (c *Controller) releaseMachineClaim(ctx context.Context, claim model.MachineClaim, byName map[string]model.Machine) {
	if !model.HasFinalizerList(claim.Metadata.Finalizers, model.FinalizerMachineClaim) {
		return
	}
	if claim.Status.EgressPolicy != "" {
		if err := c.Kube.DeleteMachineNetworkPolicy(ctx, claim.Namespace(), claim.Status.EgressPolicy); err != nil && !kube.IsNotFound(err) {
			c.Log.Error("machineclaim egress policy delete failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "error", err)
			return
		}
	}
	name := claim.Status.MachineName
	if _, ok := byName[claim.Namespace()+"/"+name]; ok && name != "" && agentplane.WantsSnapshot(claim) {
		done, err := c.ensureReleaseSnapshot(ctx, claim, name)
		if err != nil {
			c.Log.Error("machineclaim release snapshot failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "error", err)
			return
		}
		if !done {
			return
		}
	}
	if m, ok := byName[claim.Namespace()+"/"+name]; ok && name != "" && m.Metadata.Labels[model.LabelMachineClaim] == claim.Metadata.Name {
		var err error
		if claim.Spec.ReclaimPolicy == model.ReclaimRetain {
			err = c.Kube.PatchMachine(ctx, m.Namespace(), name, map[string]any{"metadata": map[string]any{"labels": map[string]any{
				model.LabelMachineClaim: nil,
				model.LabelPoolState:    nil,
				model.LabelMachinePool:  nil,
			}}})
		} else if m.Metadata.DeletionTimestamp == nil {
			err = c.Kube.DeleteMachine(ctx, m.Namespace(), name)
		}
		if err != nil && !kube.IsNotFound(err) {
			c.Log.Error("machineclaim release failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "machine", name, "error", err)
			return
		}
	}
	finals := model.RemoveFinalizer(claim.Metadata.Finalizers, model.FinalizerMachineClaim)
	if err := c.Kube.PatchMachineClaim(ctx, claim.Namespace(), claim.Metadata.Name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
		c.Log.Error("machineclaim finalizer removal failed", "namespace", claim.Namespace(), "claim", claim.Metadata.Name, "error", err)
		return
	}
	if name != "" {
		u := claim.Status.Usage
		msg := fmt.Sprintf("released %s: active %s, suspended %s, %.0f vCPU-s, %.0f GiB-s",
			name, time.Duration(u.ActiveSeconds)*time.Second, time.Duration(u.SuspendedSeconds)*time.Second, u.VCPUSeconds, u.MemoryGiBSeconds)
		if err := c.Kube.RecordEvent(ctx, claim.Namespace(), model.KindMachineClaim, claim.Metadata.Name, claim.Metadata.UID, "Released", msg, "Normal"); err != nil {
			c.Log.Debug("record claim release event failed", "claim", claim.Metadata.Name, "error", err)
		}
	}
}

// reconcileMachinePools tops every pool up to spec.replicas warm members,
// replaces warm members built from an older template, and trims extras.
// Machines in bound were claimed earlier this tick.
func (c *Controller) reconcileMachinePools(ctx context.Context, pools []model.MachinePool, machines []model.Machine, bound map[string]bool) {
	for _, pool := range pools {
		if pool.Metadata.DeletionTimestamp != nil {
			c.reconcileMachinePoolDeletion(ctx, pool, machines, bound)
			continue
		}
		status, err := c.reconcileMachinePool(ctx, pool, machines, bound)
		if err != nil {
			c.Log.Error("machinepool reconcile failed", "namespace", pool.Namespace(), "pool", pool.Metadata.Name, "error", err)
			status.Message = err.Error()
		}
		if status == pool.Status {
			continue
		}
		if err := c.Kube.PatchMachinePoolStatus(ctx, pool.Namespace(), pool.Metadata.Name, status); err != nil {
			c.Log.Error("machinepool status patch failed", "namespace", pool.Namespace(), "pool", pool.Metadata.Name, "error", err)
		}
	}
}

func (c *Controller) reconcileMachinePool(ctx context.Context, pool model.MachinePool, machines []model.Machine, bound map[string]bool) (model.MachinePoolStatus, error) {
	ns, name := pool.Namespace(), pool.Metadata.Name
	desired := max(pool.Spec.Replicas, 0)
	hash := machineSetTemplateHash(pool.Spec.Template)

	var current, outdated []model.Machine
	ready, claimed := 0, 0
	for _, m := range machines {
		if m.Namespace() != ns || m.Metadata.Labels[model.LabelMachinePool] != name || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		if m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm || bound[ns+"/"+m.Metadata.Name] {
			claimed++
			continue
		}
		if m.Metadata.Labels[model.LabelMachinePoolTemplate] != hash {
			outdated = append(outdated, m)
			continue
		}
		current = append(current, m)
		if warmReady(m) {
			ready++
		}
	}
	status := model.MachinePoolStatus{
		Replicas:      len(current),
		ReadyReplicas: ready,
		Claimed:       claimed,
		Selector:      fmt.Sprintf("%s=%s,%s=%s", model.LabelMachinePool, name, model.LabelPoolState, model.PoolStateWarm),
	}

	if !model.HasFinalizerList(pool.Metadata.Finalizers, model.FinalizerMachinePool) {
		finals := append(append([]string{}, pool.Metadata.Finalizers...), model.FinalizerMachinePool)
		if err := c.Kube.PatchMachinePool(ctx, ns, name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
			return status, fmt.Errorf("add machinepool finalizer: %w", err)
		}
	}

	for _, m := range current {
		want := warmPowerTarget(pool.Spec, m, time.Now())
		if want == "" {
			continue
		}
		patch := map[string]any{"spec": map[string]any{"powerState": want}}
		if m.Metadata.ResourceVersion != "" {
			patch["metadata"] = map[string]any{"resourceVersion": m.Metadata.ResourceVersion}
		}
		if err := c.Kube.PatchMachine(ctx, ns, m.Metadata.Name, patch); err != nil && !kube.IsConflict(err) && !kube.IsNotFound(err) {
			return status, fmt.Errorf("set warm machine %s to %s: %w", m.Metadata.Name, want, err)
		}
	}

	// Warm members serve nobody yet, so outdated ones are replaced at once
	// rather than rolled.
	for _, m := range outdated {
		if err := c.Kube.DeleteMachine(ctx, ns, m.Metadata.Name); err != nil && !kube.IsNotFound(err) {
			return status, fmt.Errorf("delete outdated warm machine %s: %w", m.Metadata.Name, err)
		}
	}
	switch {
	case len(current) < desired:
		for i := len(current); i < desired; i++ {
			if err := c.createPoolMember(ctx, pool, hash); err != nil {
				return status, err
			}
		}
	case len(current) > desired:
		// Trim booting members before ready ones.
		sort.SliceStable(current, func(i, j int) bool {
			return !warmReady(current[i]) && warmReady(current[j])
		})
		for _, m := range current[:len(current)-desired] {
			if err := c.Kube.DeleteMachine(ctx, ns, m.Metadata.Name); err != nil && !kube.IsNotFound(err) {
				return status, fmt.Errorf("delete surplus warm machine %s: %w", m.Metadata.Name, err)
			}
		}
	}
	return status, nil
}

func (c *Controller) createPoolMember(ctx context.Context, pool model.MachinePool, hash string) error {
	labels := map[string]string{}
	for k, v := range pool.Spec.Template.Labels {
		labels[k] = v
	}
	labels[model.LabelMachinePool] = pool.Metadata.Name
	labels[model.LabelMachinePoolTemplate] = hash
	labels[model.LabelPoolState] = model.PoolStateWarm
	machine := model.Machine{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachine},
		Metadata: model.ObjectMeta{Name: generateMachineSetChildName(pool.Metadata.Name), Namespace: pool.Namespace(), Labels: labels},
		Spec:     pool.Spec.Template.Spec,
	}
	if _, err := c.Kube.CreateMachine(ctx, pool.Namespace(), machine); err != nil {
		return fmt.Errorf("create warm machine for pool %s/%s: %w", pool.Namespace(), pool.Metadata.Name, err)
	}
	c.Log.Info("machinepool created warm machine", "namespace", pool.Namespace(), "pool", pool.Metadata.Name, "machine", machine.Metadata.Name)
	return nil
}

// reconcileMachinePoolDeletion deletes the pool's warm members and drops
// the finalizer once none is left. Claimed members are untouched.
func (c *Controller) reconcileMachinePoolDeletion(ctx context.Context, pool model.MachinePool, machines []model.Machine, bound map[string]bool) {
	if !model.HasFinalizerList(pool.Metadata.Finalizers, model.FinalizerMachinePool) {
		return
	}
	ns, name := pool.Namespace(), pool.Metadata.Name
	remaining := 0
	for _, m := range machines {
		if m.Namespace() != ns || m.Metadata.Labels[model.LabelMachinePool] != name ||
			m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm || bound[ns+"/"+m.Metadata.Name] {
			continue
		}
		remaining++
		if m.Metadata.DeletionTimestamp != nil {
			continue
		}
		if err := c.Kube.DeleteMachine(ctx, ns, m.Metadata.Name); err != nil && !kube.IsNotFound(err) {
			c.Log.Error("machinepool deletion: delete warm machine failed", "namespace", ns, "pool", name, "machine", m.Metadata.Name, "error", err)
			return
		}
	}
	if remaining > 0 {
		return
	}
	finals := model.RemoveFinalizer(pool.Metadata.Finalizers, model.FinalizerMachinePool)
	if err := c.Kube.PatchMachinePool(ctx, ns, name, map[string]any{"metadata": map[string]any{"finalizers": finals}}); err != nil {
		c.Log.Error("machinepool deletion: finalizer removal failed", "namespace", ns, "pool", name, "error", err)
	}
}

// ensureReleaseSnapshot reports done only once the release snapshot has
// Succeeded. The snapshot controller needs the source Machine, so the
// caller must not delete or unlabel it before then.
func (c *Controller) ensureReleaseSnapshot(ctx context.Context, claim model.MachineClaim, machine string) (bool, error) {
	name := agentplane.ReleaseSnapshotName(claim.Metadata.Name)
	snap, err := c.Kube.GetMachineSnapshot(ctx, claim.Namespace(), name)
	if err == nil {
		switch snap.Status.Phase {
		case "Succeeded":
			return true, nil
		case "Failed":
			return false, fmt.Errorf("snapshot %s failed: %s; delete it to retry", name, snap.Status.Message)
		}
		return false, nil
	}
	if !kube.IsNotFound(err) {
		return false, err
	}
	_, err = c.Kube.CreateMachineSnapshot(ctx, claim.Namespace(), model.MachineSnapshot{
		Metadata: model.ObjectMeta{Name: name, Namespace: claim.Namespace()},
		Spec:     model.MachineSnapshotSpec{MachineName: machine},
	})
	if err != nil && !kube.IsConflict(err) {
		return false, err
	}
	return false, nil
}
