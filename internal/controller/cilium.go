// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileCiliumAttach ensures a CiliumExternalWorkload exists for every
// Machine with spec.network.ciliumAttach, projects status.network.cilium,
// and patches spec.network.podUID from the CEW UID. Feature-flagged via
// Controller.CiliumAttach; deletion finalizers always run when present.
func (c *Controller) reconcileCiliumAttach(ctx context.Context, machines []model.Machine) {
	for _, m := range machines {
		if err := c.reconcileMachineCiliumAttach(ctx, m); err != nil {
			c.Log.Error("cilium attach reconcile failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("ciliumattach")
			}
		}
	}
}

func (c *Controller) reconcileMachineCiliumAttach(ctx context.Context, m model.Machine) error {
	wantAttach := c.CiliumAttach && m.Spec.Network.CiliumAttach
	hasFinalizer := model.HasFinalizerList(m.Metadata.Finalizers, model.FinalizerCiliumAttach)
	cewName := kube.ExternalWorkloadName(m.Namespace(), m.Metadata.Name)

	if m.Metadata.DeletionTimestamp != nil {
		if !hasFinalizer {
			return nil
		}
		if err := c.Kube.DeleteCiliumExternalWorkload(ctx, cewName); err != nil {
			return fmt.Errorf("delete CiliumExternalWorkload %s: %w", cewName, err)
		}
		finals := model.RemoveFinalizer(m.Metadata.Finalizers, model.FinalizerCiliumAttach)
		return c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"metadata": map[string]any{"finalizers": finals},
		})
	}

	if !wantAttach {
		if hasFinalizer {
			_ = c.Kube.DeleteCiliumExternalWorkload(ctx, cewName)
			finals := model.RemoveFinalizer(m.Metadata.Finalizers, model.FinalizerCiliumAttach)
			if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
				"metadata": map[string]any{"finalizers": finals},
			}); err != nil {
				return err
			}
		}
		return nil
	}

	if err := model.ValidateCiliumAttach(m.Spec.Network); err != nil {
		status := m.Status
		if status.Network == nil {
			status.Network = &model.MachineNetworkStatus{}
		}
		status.Network.Cilium = &model.MachineCiliumStatus{
			ExternalWorkload: cewName,
			Message:          err.Error(),
		}
		return c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status)
	}

	if !hasFinalizer {
		meta := model.AddFinalizerMeta(m.Metadata.Finalizers, model.FinalizerCiliumAttach, m.Metadata.ResourceVersion)
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": meta}); err != nil {
			return fmt.Errorf("add cilium-attach finalizer: %w", err)
		}
	}

	labels := map[string]string{
		model.LabelManagedBy:        model.ManagedByKairon,
		model.LabelMachineNamespace: m.Namespace(),
		model.LabelMachineName:      m.Metadata.Name,
	}
	for k, v := range m.Metadata.Labels {
		if k == "" {
			continue
		}
		labels[k] = v
	}
	for k, v := range m.Spec.Network.CiliumLabels {
		if k == "" {
			continue
		}
		labels[k] = v
	}

	cew, err := c.Kube.GetCiliumExternalWorkload(ctx, cewName)
	switch {
	case kube.IsNotFound(err):
		cew, err = c.Kube.CreateCiliumExternalWorkload(ctx, kube.CiliumExternalWorkload{
			APIVersion: "cilium.io/v2",
			Kind:       "CiliumExternalWorkload",
			Metadata: kube.ObjectMetaLite{
				Name:   cewName,
				Labels: labels,
			},
			Spec: map[string]any{},
		})
		if err != nil {
			return fmt.Errorf("create CiliumExternalWorkload %s: %w", cewName, err)
		}
	case err != nil:
		return err
	case labelsChanged(cew.Metadata.Labels, labels):
		if err := c.Kube.PatchCiliumExternalWorkload(ctx, cewName, map[string]any{
			"metadata": map[string]any{"labels": labels},
		}); err != nil {
			return fmt.Errorf("patch CiliumExternalWorkload labels: %w", err)
		}
		cew.Metadata.Labels = labels
	}

	ipv4 := cew.Status.IP
	if ipv4 == "" && len(cew.Status.IPs) > 0 {
		ipv4 = cew.Status.IPs[0]
	}
	ciliumStatus := &model.MachineCiliumStatus{
		ExternalWorkload:    cewName,
		ExternalWorkloadUID: cew.Metadata.UID,
		Identity:            cew.Status.ID,
		IPv4:                ipv4,
	}
	status := m.Status
	if status.Network == nil {
		status.Network = &model.MachineNetworkStatus{}
	}
	status.Network.Cilium = ciliumStatus
	if err := c.Kube.PatchMachineStatus(ctx, m.Namespace(), m.Metadata.Name, status); err != nil {
		return err
	}

	if cew.Metadata.UID != "" && m.Spec.Network.PodUID != cew.Metadata.UID {
		return c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{
			"spec": map[string]any{
				"network": map[string]any{"podUID": cew.Metadata.UID},
			},
		})
	}
	return nil
}

func labelsChanged(have, want map[string]string) bool {
	if len(have) != len(want) {
		return true
	}
	for k, v := range want {
		if have[k] != v {
			return true
		}
	}
	return false
}

// reconcileCiliumPolicySync mirrors MachineNetworkPolicy intent onto a
// namespaced CiliumNetworkPolicy when spec.cilium.sync is true.
func (c *Controller) reconcileCiliumPolicySync(ctx context.Context) {
	if !c.CiliumPolicySync {
		return
	}
	policies, err := c.Kube.ListMachineNetworkPolicies(ctx)
	if err != nil {
		if !kube.IsNotFound(err) {
			c.Log.Error("list MachineNetworkPolicies for cilium sync failed", "error", err)
		}
		return
	}
	for _, p := range policies {
		if err := c.reconcileMachineNetworkPolicyCilium(ctx, p); err != nil {
			c.Log.Error("cilium policy sync failed", "namespace", p.Namespace(), "policy", p.Metadata.Name, "error", err)
			if c.Metrics != nil {
				c.Metrics.ObserveReconcileItemError("ciliumpolicysync")
			}
		}
	}
}

func (c *Controller) reconcileMachineNetworkPolicyCilium(ctx context.Context, p model.MachineNetworkPolicy) error {
	syncOn := p.Spec.Cilium != nil && p.Spec.Cilium.Sync
	cnpName := p.Metadata.Name
	if p.Spec.Cilium != nil && p.Spec.Cilium.PolicyName != "" {
		cnpName = p.Spec.Cilium.PolicyName
	}
	ref := p.Namespace() + "/" + cnpName
	hasFinalizer := model.HasFinalizerList(p.Metadata.Finalizers, model.FinalizerCiliumPolicySync)

	if p.Metadata.DeletionTimestamp != nil || !syncOn {
		if hasFinalizer || p.Status.CiliumNetworkPolicyRef != "" {
			_ = c.Kube.DeleteCiliumNetworkPolicy(ctx, p.Namespace(), cnpName)
			if p.Status.CiliumNetworkPolicyRef != "" && p.Status.CiliumNetworkPolicyRef != ref {
				parts := strings.SplitN(p.Status.CiliumNetworkPolicyRef, "/", 2)
				if len(parts) == 2 {
					_ = c.Kube.DeleteCiliumNetworkPolicy(ctx, parts[0], parts[1])
				}
			}
		}
		if hasFinalizer {
			finals := model.RemoveFinalizer(p.Metadata.Finalizers, model.FinalizerCiliumPolicySync)
			if err := c.Kube.PatchMachineNetworkPolicy(ctx, p.Namespace(), p.Metadata.Name, map[string]any{
				"metadata": map[string]any{"finalizers": finals},
			}); err != nil {
				return err
			}
		}
		if p.Status.CiliumNetworkPolicyRef != "" || p.Status.CiliumSyncMessage != "" {
			st := p.Status
			st.CiliumNetworkPolicyRef = ""
			st.CiliumSyncMessage = ""
			return c.Kube.PatchMachineNetworkPolicyStatus(ctx, p.Namespace(), p.Metadata.Name, st)
		}
		return nil
	}

	if !hasFinalizer {
		finals := append(append([]string{}, p.Metadata.Finalizers...), model.FinalizerCiliumPolicySync)
		if err := c.Kube.PatchMachineNetworkPolicy(ctx, p.Namespace(), p.Metadata.Name, map[string]any{
			"metadata": map[string]any{"finalizers": finals},
		}); err != nil {
			return fmt.Errorf("add cilium-policy-sync finalizer: %w", err)
		}
	}

	specBody, err := buildCiliumNetworkPolicySpec(p)
	if err != nil {
		st := p.Status
		st.CiliumSyncMessage = err.Error()
		return c.Kube.PatchMachineNetworkPolicyStatus(ctx, p.Namespace(), p.Metadata.Name, st)
	}

	labels := map[string]string{
		model.LabelManagedBy: model.ManagedByKairon,
	}
	existing, err := c.Kube.GetCiliumNetworkPolicy(ctx, p.Namespace(), cnpName)
	switch {
	case kube.IsNotFound(err):
		_, err = c.Kube.CreateCiliumNetworkPolicy(ctx, p.Namespace(), kube.CiliumNetworkPolicy{
			APIVersion: "cilium.io/v2",
			Kind:       "CiliumNetworkPolicy",
			Metadata: kube.ObjectMetaLite{
				Name:      cnpName,
				Namespace: p.Namespace(),
				Labels:    labels,
			},
			Spec: specBody,
		})
		if err != nil {
			return fmt.Errorf("create CiliumNetworkPolicy: %w", err)
		}
	case err != nil:
		return err
	default:
		_ = existing
		if err := c.Kube.PatchCiliumNetworkPolicy(ctx, p.Namespace(), cnpName, map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec":     specBody,
		}); err != nil {
			return fmt.Errorf("patch CiliumNetworkPolicy: %w", err)
		}
	}

	st := p.Status
	st.CiliumNetworkPolicyRef = ref
	st.CiliumSyncMessage = "synced"
	return c.Kube.PatchMachineNetworkPolicyStatus(ctx, p.Namespace(), p.Metadata.Name, st)
}

// buildCiliumNetworkPolicySpec prefers spec.cnp when set; otherwise translates
// VmNetworkPolicy into a CNP-shaped endpointSelector + egress rules.
func buildCiliumNetworkPolicySpec(p model.MachineNetworkPolicy) (map[string]any, error) {
	if len(p.Spec.CNP) > 0 {
		if spec, ok := p.Spec.CNP["spec"].(map[string]any); ok {
			return spec, nil
		}
		// Treat the whole document as the CNP spec body when no nested "spec".
		out := map[string]any{}
		for k, v := range p.Spec.CNP {
			if k == "apiVersion" || k == "kind" || k == "metadata" {
				continue
			}
			out[k] = v
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("spec.cnp has no usable policy body")
		}
		return out, nil
	}

	matchLabels := map[string]string{}
	if p.Spec.MachineName != "" {
		matchLabels[model.LabelMachineName] = p.Spec.MachineName
		matchLabels[model.LabelMachineNamespace] = p.Namespace()
	} else {
		for k, v := range p.Spec.Selector {
			matchLabels[k] = v
		}
	}
	if len(matchLabels) == 0 {
		return nil, fmt.Errorf("cilium sync requires machineName or selector (or spec.cnp)")
	}

	spec := map[string]any{
		"endpointSelector": map[string]any{
			"matchLabels": matchLabels,
		},
	}
	pol := p.Spec.Policy
	var egress []map[string]any
	if len(pol.AllowCidrs) > 0 || len(pol.AllowPorts) > 0 {
		rule := map[string]any{}
		if len(pol.AllowCidrs) > 0 {
			rule["toCIDR"] = pol.AllowCidrs
		}
		if ports := translateAllowPorts(pol.AllowPorts); len(ports) > 0 {
			rule["toPorts"] = []map[string]any{{"ports": ports}}
		}
		egress = append(egress, rule)
	}
	if pol.DefaultAllow && len(egress) == 0 {
		egress = append(egress, map[string]any{})
	}
	if !pol.DefaultAllow || len(egress) > 0 {
		spec["egress"] = egress
	}
	return spec, nil
}

func translateAllowPorts(rules []string) []map[string]any {
	var out []map[string]any
	for _, raw := range rules {
		proto, portStr, ok := strings.Cut(raw, "/")
		if !ok {
			continue
		}
		proto = strings.ToLower(strings.TrimSpace(proto))
		switch proto {
		case "tcp", "udp", "sctp":
			proto = strings.ToUpper(proto)
		default:
			continue
		}
		portStr = strings.TrimSpace(portStr)
		if _, err := strconv.ParseUint(portStr, 10, 16); err != nil {
			continue
		}
		out = append(out, map[string]any{"port": portStr, "protocol": proto})
	}
	return out
}
