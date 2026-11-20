// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"fmt"
	"regexp"
)

// Annotation keys. Empty values keep today's behaviour: Spec.Tenant stays
// metadata until an agent pool or require-tenant asks for enforcement.
const (
	AnnAgentPool     = "kairon.zyvor.dev/agent-pool"
	AnnRequireTenant = "kairon.zyvor.dev/require-tenant"
	AnnImageSign     = "kairon.zyvor.dev/image-signature"
	AnnConfidential  = "kairon.zyvor.dev/confidential"
	AnnGPUCount      = "kairon.zyvor.dev/gpu-count"
	AnnLiveMigrate   = "kairon.zyvor.dev/live-migrate"
	AnnEgress        = "kairon.zyvor.dev/egress-allowlist"
	AnnGateway       = "kairon.zyvor.dev/gateway"
	AnnHypervisor    = "kairon.zyvor.dev/hypervisor"

	// AnnAttestationVerified is written by the attestation verifier with
	// the kind (sev-snp or tdx) whose report it accepted for this Machine.
	AnnAttestationVerified = "kairon.zyvor.dev/attestation-verified"
	// AnnAttestationNonce is the controller's per-round nonce; the guest
	// binds it into REPORT_DATA (see internal/attest.ReportData).
	AnnAttestationNonce = "kairon.zyvor.dev/attestation-nonce"
	// AnnAttestationReport is the base64 report or quote for the current
	// nonce. Anyone may write it: it is trusted only after verification.
	AnnAttestationReport = "kairon.zyvor.dev/attestation-report"
	AnnAttestationError  = "kairon.zyvor.dev/attestation-error"
	// LabelConfidentialCapable is the node label kairon-node publishes
	// with the confidential kind its host kernel can run.
	LabelConfidentialCapable = "kairon.zyvor.dev/confidential-capable"
	LabelTenant              = "kairon.zyvor.dev/tenant"
)

var tenantName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Principal is the MCP or webhook caller. An empty Tenant is the
// cluster-admin path and is not tenant-scoped. A set Tenant can only
// see objects with the same Spec.Tenant, and only in Namespaces when
// that list is non-empty.
type Principal struct {
	Name       string
	Tenant     string
	Namespaces []string
}

func ValidTenant(tenant string) error {
	if tenant == "" {
		return fmt.Errorf("tenant is required")
	}
	if !tenantName.MatchString(tenant) {
		return fmt.Errorf("tenant %q must match %s", tenant, tenantName.String())
	}
	return nil
}

// CheckTenant enforces the caller scope. A principal with no tenant is
// unrestricted so existing single-tenant clusters do not break.
func CheckTenant(p Principal, namespace, tenant string) error {
	if p.Tenant == "" {
		return nil
	}
	if err := ValidTenant(p.Tenant); err != nil {
		return fmt.Errorf("caller: %w", err)
	}
	if tenant == "" {
		return fmt.Errorf("caller tenant %q cannot read an object with an empty tenant", p.Tenant)
	}
	if tenant != p.Tenant {
		return fmt.Errorf("caller tenant %q cannot access tenant %q", p.Tenant, tenant)
	}
	if len(p.Namespaces) == 0 {
		return nil
	}
	for _, ns := range p.Namespaces {
		if ns == namespace {
			return nil
		}
	}
	return fmt.Errorf("caller tenant %q is not allowed in namespace %q", p.Tenant, namespace)
}
