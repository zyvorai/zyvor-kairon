// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"fmt"
	"strconv"
	"strings"
)

// MachineAdmission is the slice of a Machine the webhook can judge
// without a client. Zero values preserve today's allow path.
type MachineAdmission struct {
	Namespace     string
	Name          string
	Tenant        string
	Annotations   map[string]string
	ImageDigest   string
	DataplaneMode string
	DeviceClaims  int
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// AdmitMachine applies agent-pool, tenant, image, confidential and GPU
// rules. Machines that do not opt in are allowed.
func AdmitMachine(m MachineAdmission) error {
	ann := m.Annotations
	if ann == nil {
		ann = map[string]string{}
	}
	agentPool := truthy(ann[AnnAgentPool])
	if agentPool || truthy(ann[AnnRequireTenant]) {
		if err := ValidTenant(m.Tenant); err != nil {
			return fmt.Errorf("spec.tenant: %w", err)
		}
	}
	if agentPool {
		if err := AdmitImage(true, m.ImageDigest, ann[AnnImageSign]); err != nil {
			return err
		}
		if strings.TrimSpace(ann[AnnEgress]) == "" {
			return fmt.Errorf("agent pool requires %s", AnnEgress)
		}
		mode := m.DataplaneMode
		if mode == "" {
			mode = "ebpf"
		}
		if mode != "ebpf" {
			return fmt.Errorf("agent pool dataplaneMode must be ebpf")
		}
		hv := ann[AnnHypervisor]
		if hv == "" {
			hv = "firecracker"
		}
		if err := ValidateClaim(ClaimRequest{
			Pool: "admission", Name: m.Name, Tenant: m.Tenant, TTLSec: 60, Hypervisor: hv,
			Egress: PolicyIntent{Name: "admission", Namespace: m.Namespace, AllowFQDNs: strings.Split(ann[AnnEgress], ",")},
		}); err != nil {
			return err
		}
	}
	if req := ann[AnnConfidential]; req != "" {
		if err := AdmitConfidential(req, Attestation{Kind: req, ReportValid: true, NodeCapable: true}); err != nil {
			return err
		}
	}
	if raw := ann[AnnGPUCount]; raw != "" {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", AnnGPUCount, err)
		}
		if err := AdmitGPU(GPUClaim{Count: n, LiveMigrate: truthy(ann[AnnLiveMigrate])}); err != nil {
			return err
		}
	} else if m.DeviceClaims > 0 && truthy(ann[AnnLiveMigrate]) {
		return fmt.Errorf("refusing live migration of a Machine with device claims")
	}
	return nil
}
