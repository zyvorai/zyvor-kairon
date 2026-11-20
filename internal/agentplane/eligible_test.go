// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestEligibleWarmFiltersTenantAndHypervisor(t *testing.T) {
	claim := model.MachineClaim{
		Metadata: model.ObjectMeta{Annotations: map[string]string{AnnHypervisor: "firecracker", AnnSnapshotOnRelease: "true"}},
		Spec:     model.MachineClaimSpec{Labels: map[string]string{"kairon.zyvor.dev/tenant": "acme"}},
	}
	ok := model.Machine{Spec: model.MachineSpec{Tenant: "acme"}, Status: model.MachineStatus{Phase: "Running"}, Metadata: model.ObjectMeta{Annotations: map[string]string{AnnHypervisor: "firecracker"}, Labels: map[string]string{model.LabelPoolState: model.PoolStateWarm}}}
	if !EligibleWarm(claim, ok) {
		t.Fatal("expected eligible")
	}
	bad := ok
	bad.Spec.Tenant = "other"
	if EligibleWarm(claim, bad) {
		t.Fatal("tenant mismatch should be ineligible")
	}
	if !WantsSnapshot(claim) || ReleaseSnapshotName("job-1") != "job-1-release" {
		t.Fatal("snapshot name")
	}
	ann := StatusAnnotations(ConfidentialStatus{Kind: "sev-snp", Sealed: true}, "agents", 1, false)
	if ann["kairon.zyvor.dev/confidential-sealed"] != "true" || ann[AnnGateway] != "agents" {
		t.Fatalf("%#v", ann)
	}
}
