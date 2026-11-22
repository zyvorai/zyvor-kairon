// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"testing"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/model"
)

func TestReconcileAgentStatusPatchesOnlyChangedKeys(t *testing.T) {
	c, fake := newPoolTestController(t)
	node := model.Node{Metadata: model.ObjectMeta{Name: "n1", Labels: map[string]string{agentplane.LabelConfidentialCapable: "sev-snp"}}}
	mk := func(name string, ann map[string]string) model.Machine {
		return model.Machine{Metadata: model.ObjectMeta{Name: name, Namespace: "prod", Annotations: ann}, Spec: model.MachineSpec{NodeName: "n1"}}
	}
	machines := []model.Machine{
		mk("unsealed", map[string]string{agentplane.AnnConfidential: "sev-snp"}),
		mk("sealed", map[string]string{agentplane.AnnConfidential: "sev-snp", agentplane.AnnAttestationVerified: "sev-snp"}),
		mk("steady", map[string]string{agentplane.AnnConfidential: "sev-snp", agentplane.AnnAttestationVerified: "sev-snp", "kairon.zyvor.dev/confidential-sealed": "true"}),
		mk("wrong-kind", map[string]string{agentplane.AnnConfidential: "tdx"}),
		mk("plain", nil),
	}
	c.reconcileAgentStatus(t.Context(), machines, []model.Node{node})

	ann := func(name string) map[string]any {
		a, _ := fake.machinePatch[name]["annotations"].(map[string]any)
		return a
	}
	if a := ann("unsealed"); a["kairon.zyvor.dev/confidential-sealed"] != "false" || a["kairon.zyvor.dev/confidential-reason"] == nil {
		t.Fatalf("unsealed = %v", a)
	}
	if a := ann("sealed"); a["kairon.zyvor.dev/confidential-sealed"] != "true" {
		t.Fatalf("sealed = %v", a)
	}
	if a := ann("wrong-kind"); a["kairon.zyvor.dev/confidential-sealed"] != "false" {
		t.Fatalf("wrong-kind = %v", a)
	}
	if _, ok := fake.machinePatch["steady"]; ok {
		t.Fatal("steady Machine was patched")
	}
	if _, ok := fake.machinePatch["plain"]; ok {
		t.Fatal("Machine without opt-in was patched")
	}
}
