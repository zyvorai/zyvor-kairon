// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"fmt"
	"strings"
)

// Attestation is the node fact admission cannot invent. ReportValid
// is the verifier's result, not a claim from the guest.
type Attestation struct {
	Kind        string `json:"kind"`
	ReportValid bool   `json:"reportValid"`
	NodeCapable bool   `json:"nodeCapable"`
}

func AdmitConfidential(requested string, node Attestation) error {
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "" || req == "none" {
		return nil
	}
	if req != "sev-snp" && req != "tdx" {
		return fmt.Errorf("confidential %q is not sev-snp or tdx", requested)
	}
	if !node.NodeCapable || strings.ToLower(node.Kind) != req {
		return fmt.Errorf("node cannot attest %s", req)
	}
	if !node.ReportValid {
		return fmt.Errorf("attestation report for %s is not valid", req)
	}
	return nil
}
