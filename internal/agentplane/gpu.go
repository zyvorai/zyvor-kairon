// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// GPUClaim is a whole-GPU or MIG request. Live migration of a
// passthrough device is refused, matching the VFIO preflight.
type GPUClaim struct {
	Count       int    `json:"count"`
	MIG         string `json:"mig,omitempty"`
	LiveMigrate bool   `json:"liveMigrate,omitempty"`
}

func AdmitGPU(c GPUClaim) error {
	if c.Count < 0 {
		return fmt.Errorf("gpu count cannot be negative")
	}
	if c.Count == 0 && c.MIG == "" {
		return nil
	}
	if c.Count == 0 && c.MIG != "" {
		return fmt.Errorf("mig profile requires gpu count >= 1")
	}
	if c.LiveMigrate {
		return fmt.Errorf("refusing live migration of a GPU claim: VFIO state does not move with guest RAM")
	}
	return nil
}
