// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"strconv"
	"strings"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/model"
)

// reconcileAgentStatus projects agentplane.StatusAnnotations onto opted-in
// Machines. Only keys whose value changed are patched, so a steady
// Machine costs no apiserver write.
func (c *Controller) reconcileAgentStatus(ctx context.Context, machines []model.Machine, nodes []model.Node) {
	nodeLabels := make(map[string]map[string]string, len(nodes))
	for _, n := range nodes {
		nodeLabels[n.Metadata.Name] = n.Metadata.Labels
	}
	for _, m := range machines {
		if m.Metadata.DeletionTimestamp != nil {
			continue
		}
		want := agentStatusFor(m, nodeLabels[m.Spec.NodeName])
		patch := map[string]any{}
		for k, v := range want {
			if m.Metadata.Annotations[k] != v {
				patch[k] = v
			}
		}
		if len(patch) == 0 {
			continue
		}
		if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"annotations": patch}}); err != nil {
			c.Log.Error("machine agent status patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
		}
	}
}

// agentStatusFor returns the status annotations for one Machine, or nil
// when it opted into nothing the agent plane projects.
func agentStatusFor(m model.Machine, nodeLabels map[string]string) map[string]string {
	ann := m.Metadata.Annotations
	requested := strings.ToLower(strings.TrimSpace(ann[agentplane.AnnConfidential]))
	gateway := ann[agentplane.AnnGateway]
	gpus, _ := strconv.Atoi(strings.TrimSpace(ann[agentplane.AnnGPUCount]))
	if requested == "" && gateway == "" && gpus <= 0 {
		return nil
	}
	var conf agentplane.ConfidentialStatus
	if requested != "" {
		if m.Spec.NodeName == "" {
			conf = agentplane.ConfidentialStatus{Kind: requested, Reason: "not scheduled"}
		} else {
			capable := nodeLabels[agentplane.LabelConfidentialCapable]
			conf = agentplane.ProjectConfidential(requested, agentplane.Attestation{
				Kind:        capable,
				NodeCapable: capable != "",
				ReportValid: ann[agentplane.AnnAttestationVerified] == requested,
			})
		}
	}
	return agentplane.StatusAnnotations(conf, gateway, gpus, truthyAnn(ann[agentplane.AnnLiveMigrate]))
}

func truthyAnn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
