// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"fmt"
	"strings"
)

// Guest-side tools an agent inside a claim must never receive.
var forbiddenGuestTools = map[string]struct{}{
	"delete_machine":  {},
	"fork_machine":    {},
	"claim_machine":   {},
	"release_claim":   {},
	"set_power_state": {},
}

var agentHypervisors = map[string]struct{}{
	"firecracker":      {},
	"cloud-hypervisor": {},
	"qemu":             {},
	"fluxvm":           {},
}

// ClaimRequest is the sealed-computer contract on top of MachinePool.
// Egress is mandatory. TTL is bounded. The guest tool list cannot
// include control-plane verbs.
type ClaimRequest struct {
	Pool       string       `json:"pool"`
	Name       string       `json:"name"`
	Tenant     string       `json:"tenant"`
	TTLSec     int          `json:"ttlSec"`
	Hypervisor string       `json:"hypervisor"`
	Egress     PolicyIntent `json:"egress"`
	Tools      []string     `json:"tools,omitempty"`
}

func ValidateClaim(c ClaimRequest) error {
	if strings.TrimSpace(c.Pool) == "" || strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("pool and name are required")
	}
	if err := ValidTenant(c.Tenant); err != nil {
		return err
	}
	if c.TTLSec < 30 || c.TTLSec > 86400 {
		return fmt.Errorf("ttlSec must be between 30 and 86400")
	}
	hv := strings.ToLower(strings.TrimSpace(c.Hypervisor))
	if hv == "" {
		hv = "firecracker"
	}
	if _, ok := agentHypervisors[hv]; !ok {
		return fmt.Errorf("hypervisor %q is not one of firecracker, cloud-hypervisor, qemu, fluxvm", c.Hypervisor)
	}
	if _, err := CompilePolicy(c.Egress); err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	for _, t := range c.Tools {
		if _, bad := forbiddenGuestTools[t]; bad {
			return fmt.Errorf("guest tool %q is control-plane only", t)
		}
	}
	return nil
}
