// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"github.com/zyvorai/kairon/internal/model"
	"github.com/zyvorai/kairon/internal/preempt"
	"github.com/zyvorai/kairon/internal/tenantfence"
)

// OfflineCheck is one Machine admission rule that needs no cluster state,
// with the error it produced (nil when the Machine passes).
type OfflineCheck struct {
	Rule string
	Err  error
}

// OfflineMachineChecks runs, without a Controller, Kube client or request,
// the stateless validations validateMachineCreate performs on a Machine
// CREATE, and returns the failures (all of them, not just the first).
//
// It calls the same functions in the same grouping as validateMachineCreate.
// It deliberately leaves out the checks that need the cluster or the caller:
// instance type lookup, agent image verification (needs the Secret-backed
// trust config), the attestation annotation write check (needs the user),
// agentplane.AdmitMachine (needs the namespace) and MachineQuota. When a new
// stateless check is added to validateMachineCreate, add it here too.
func OfflineMachineChecks(m model.Machine) []OfflineCheck {
	checks := []OfflineCheck{
		{"image-source", model.ValidateImageSource(m.Spec.Image)},
		{"resource-quantities", validateResourceQuantities(m.Spec.Resources)},
		{"network-mac", validateNetworkMAC(m.Spec.Network)},
		{"disks", model.ValidateDisks(m.Spec.Disks)},
		{"root-image", model.ValidateRootImage(m.Spec.Image)},
		{"cdroms", model.ValidateCdroms(m.Spec.Cdroms)},
		{"topology", model.ValidateTopology(m.Spec)},
		{"placement", model.ValidatePlacement(m.Spec)},
		{"sandbox-oci", model.ValidateSandboxOCI(m.Spec)},
		{"accelerators", model.ValidateAccelerators(m.Spec, m.Metadata.Annotations)},
		{"elastic-memory", model.ValidateElasticMemory(m.Spec)},
		{"extra-interfaces", model.ValidateExtraInterfaces(m.Spec.Network)},
		{"dataplane-mode", model.ValidateDataplaneMode(m.Spec.Network)},
		{"egress", model.ValidateEgress(m.Spec.Network)},
		{"cilium-attach", model.ValidateCiliumAttach(m.Spec.Network)},
		{"virtual-network", model.ValidateMachineVirtualNetwork(m.Spec.Network)},
		{"lifecycle", model.ValidateLifecycle(m.Spec.Lifecycle)},
		{"health-check", model.ValidateHealthCheck(m.Spec)},
		{"tenant", tenantfence.CheckCreate(m.Spec.Tenant, m.Metadata.Annotations)},
		{"preempt-policy", preempt.ValidatePolicy(m.Metadata.Annotations[preempt.AnnPolicy])},
		{"instance-type-ref", model.ValidateInstanceTypeRef(m.Spec)},
	}
	var failed []OfflineCheck
	for _, c := range checks {
		if c.Err != nil {
			failed = append(failed, c)
		}
	}
	return failed
}
