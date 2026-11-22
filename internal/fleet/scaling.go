// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package fleet

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/scheduler"
)

func ScaleRecommendation(current int, utilization float64, s AutoscalerSpec) int {
	desired := int(math.Ceil(float64(current) * utilization / s.TargetCPUPercent))
	if desired < s.MinReplicas {
		desired = s.MinReplicas
	}
	if desired > s.MaxReplicas {
		desired = s.MaxReplicas
	}
	if desired > current+s.MaxStep {
		desired = current + s.MaxStep
	}
	if desired < current-s.MaxStep {
		desired = current - s.MaxStep
	}
	if desired < s.MinReplicas {
		desired = s.MinReplicas
	}
	if desired > s.MaxReplicas {
		desired = s.MaxReplicas
	}
	return desired
}
func (e *Engine) autoscale(ctx context.Context, o *model.FleetResource, machines []model.Machine) error {
	s, _ := decode[AutoscalerSpec](o.Spec)
	ns := o.Metadata.Namespace
	current := 0
	rv := ""
	label := model.LabelMachineSet
	if s.TargetKind == "MachineSet" {
		target, err := e.Kube.GetMachineSet(ctx, ns, s.TargetName)
		if err != nil {
			return err
		}
		if target.Metadata.DeletionTimestamp != nil {
			return fmt.Errorf("target is deleting")
		}
		current = target.Spec.Replicas
		rv = target.Metadata.ResourceVersion
	} else {
		target, err := e.Kube.GetMachinePool(ctx, ns, s.TargetName)
		if err != nil {
			return err
		}
		if target.Metadata.DeletionTimestamp != nil {
			return fmt.Errorf("target is deleting")
		}
		current = target.Spec.Replicas
		rv = target.Metadata.ResourceVersion
		label = model.LabelMachinePool
	}
	// Require exactly one scaler per target; conflicting controllers must not fight.
	scalers, err := e.Kube.ListFleet(ctx, ns, "machineautoscalers")
	if err != nil {
		return err
	}
	for _, other := range scalers {
		if other.Metadata.UID == o.Metadata.UID {
			continue
		}
		candidate, err := decode[AutoscalerSpec](other.Spec)
		if err == nil && candidate.TargetName == s.TargetName && candidate.TargetKind == s.TargetKind && other.Metadata.DeletionTimestamp == nil {
			return fmt.Errorf("multiple autoscalers target this fleet")
		}
	}
	samples, err := e.Metrics.CPU(ctx, ns, e.now())
	if err != nil {
		return err
	}
	var targetMachines []model.Machine
	var total float64
	for _, m := range machines {
		if m.Namespace() != ns || m.Metadata.Labels[label] != s.TargetName || m.Metadata.DeletionTimestamp != nil {
			continue
		}
		if s.TargetKind == "MachinePool" && m.Metadata.Labels[model.LabelPoolState] != model.PoolStateWarm {
			continue
		}
		if m.Status.Phase != "Running" {
			return fmt.Errorf("target fleet is not fully ready")
		}
		value, ok := samples[m.Metadata.Name]
		if !ok {
			return fmt.Errorf("missing or stale usage for %s", m.Metadata.Name)
		}
		cpu, err := model.ParseVCPUs(m.Spec.Resources.CPU)
		if err != nil {
			return err
		}
		total += value / float64(cpu)
		targetMachines = append(targetMachines, m)
	}
	if len(targetMachines) != current || current < 1 {
		return fmt.Errorf("target is converging or empty; metric scaling deferred")
	}
	desired := ScaleRecommendation(current, total/float64(len(targetMachines)), s)
	now := e.now()
	o.Status.LastSampleTime = &now
	if desired == current {
		o.Status.RecommendationSince = nil
		o.Status.DesiredReplicas = current
		e.status(o, "Stable", "replicas are within target utilization")
		return nil
	}
	if o.Status.DesiredReplicas != desired || o.Status.RecommendationSince == nil {
		o.Status.DesiredReplicas = desired
		o.Status.RecommendationSince = &now
		e.status(o, "Stabilizing", "waiting for sustained recommendation")
		return nil
	}
	if now.Sub(*o.Status.RecommendationSince) < time.Duration(s.StabilizationSeconds)*time.Second || (o.Status.LastActionTime != nil && now.Sub(*o.Status.LastActionTime) < time.Duration(s.StabilizationSeconds)*time.Second) {
		return nil
	}
	if desired < current {
		if e.AdmitScaleDown == nil {
			return fmt.Errorf("scale-down budget guard is not configured")
		}
		if err := e.AdmitScaleDown(targetMachines, current-desired); err != nil {
			return err
		}
		// Do not shrink pools while any claim is active: the pool controller refills
		// warm members independently of claimed capacity.
		if s.TargetKind == "MachinePool" {
			claims, err := e.Kube.ListMachineClaimsNamespace(ctx, ns)
			if err != nil {
				return err
			}
			for _, claim := range claims {
				if claim.Spec.PoolName == s.TargetName && claim.Status.Phase == "Bound" {
					return fmt.Errorf("pool has active claims; scale-down deferred")
				}
			}
		}
	}
	patch := map[string]any{"metadata": map[string]any{"resourceVersion": rv}, "spec": map[string]int{"replicas": desired}}
	if s.TargetKind == "MachineSet" {
		err = e.Kube.PatchMachineSet(ctx, ns, s.TargetName, patch)
	} else {
		err = e.Kube.PatchMachinePool(ctx, ns, s.TargetName, patch)
	}
	if err != nil {
		return err
	}
	o.Status.LastActionTime = &now
	o.Status.RecommendationSince = nil
	e.status(o, "Scaling", fmt.Sprintf("replicas %d -> %d", current, desired))
	return nil
}
func nodePressure(n model.Node, l scheduler.NodeLoad) (float64, error) {
	cpu, err := model.ParseVCPUs(n.Status.Allocatable["cpu"])
	if err != nil {
		return 0, err
	}
	mem, err := model.ParseMemoryMiB(n.Status.Allocatable["memory"])
	if err != nil {
		return 0, err
	}
	return math.Max(float64(l.CPUMilli)/(float64(cpu)*1000), float64(l.MemoryMiB)/float64(mem)) * 100, nil
}
func (e *Engine) balance(ctx context.Context, o *model.FleetResource, machines []model.Machine, nodes []model.Node, migrations []model.MachineMigration) error {
	s, _ := decode[BalanceSpec](o.Spec)
	now := e.now()
	if o.Status.LastActionTime != nil && now.Sub(*o.Status.LastActionTime) < time.Duration(s.CooldownSeconds)*time.Second {
		return nil
	}
	load := scheduler.BuildNodeLoad(machines)
	byNode := map[string]model.Node{}
	for _, n := range nodes {
		byNode[n.Metadata.Name] = n
	}
	for _, m := range machines {
		if !selected(*o, s.Selector, m) || m.Status.Phase != "Running" || m.DesiredPowerState() != "Running" || hasMigration(m, migrations) || e.acted[m.Namespace()+"/"+m.Metadata.Name] || len(m.Spec.DeviceClaims) > 0 || m.Spec.Resources.CPUSet != "" || m.Spec.Resources.CPUPinning {
			continue
		}
		source, ok := byNode[m.Spec.NodeName]
		if !ok {
			continue
		}
		beforeSource, err := nodePressure(source, load[m.Spec.NodeName])
		if err != nil {
			continue
		}
		candidates := append([]model.Node(nil), nodes...)
		for i := range candidates {
			if candidates[i].Metadata.Name == m.Spec.NodeName {
				candidates[i].Spec.Unschedulable = true
			}
		}
		remaining := make([]model.Machine, 0, len(machines))
		for _, v := range machines {
			if v.Metadata.UID != m.Metadata.UID {
				remaining = append(remaining, v)
			}
		}
		remainingLoad := scheduler.BuildNodeLoad(remaining)
		targetName, err := e.Scheduler.Choose(m, candidates, remaining, remainingLoad, "")
		if err != nil {
			continue
		}
		target := byNode[targetName]
		beforeTarget, err := nodePressure(target, load[targetName])
		if err != nil {
			continue
		}
		cpu, err := model.ParseVCPUs(m.Spec.Resources.CPU)
		if err != nil {
			return err
		}
		mem, err := model.ParseMemoryMiB(m.Spec.Resources.Memory)
		if err != nil {
			return err
		}
		targetLoad := remainingLoad[targetName]
		targetLoad.CPUMilli += int64(cpu) * 1000
		targetLoad.MemoryMiB += mem
		afterTarget, err := nodePressure(target, targetLoad)
		if err != nil {
			continue
		}
		afterSource, err := nodePressure(source, remainingLoad[m.Spec.NodeName])
		if err != nil {
			continue
		}
		gain := math.Max(beforeSource, beforeTarget) - math.Max(afterSource, afterTarget)
		if gain < s.MinImprovementPercent {
			continue
		}
		e.status(o, "Recommendation", fmt.Sprintf("%s: %s -> %s improves reserved-resource pressure by %.1f points", m.Metadata.Name, m.Spec.NodeName, targetName, gain))
		if s.DryRun {
			return nil
		}
		if err := e.disrupt(m); err != nil {
			return err
		}
		name := childName(*o, fmt.Sprintf("balance/%s/%d", m.Metadata.UID, now.Unix()/s.CooldownSeconds))
		_, err = e.Kube.CreateMachineMigration(ctx, m.Namespace(), model.MachineMigration{TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindMachineMigration}, Metadata: childMeta(*o, name), Spec: model.MachineMigrationSpec{MachineName: m.Metadata.Name, TargetNode: targetName, Strategy: s.Strategy}})
		if kube.IsConflict(err) {
			existing, getErr := e.Kube.GetMachineMigration(ctx, m.Namespace(), name)
			if getErr != nil {
				return getErr
			}
			if !owns(*o, existing.Metadata) {
				return fmt.Errorf("foreign migration collision")
			}
			err = nil
		}
		if err != nil {
			return err
		}
		e.acted[m.Namespace()+"/"+m.Metadata.Name] = true
		o.Status.LastActionTime = &now
		return nil // one migration per policy per cooldown
	}
	e.status(o, "Stable", "no eligible move improves reserved-resource pressure enough")
	return nil
}
