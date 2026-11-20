// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

// ConfidentialStatus is what Machine status should show. Sealed is
// false until the node report matches the request.
type ConfidentialStatus struct {
	Kind   string `json:"kind,omitempty"`
	Sealed bool   `json:"sealed"`
	Reason string `json:"reason,omitempty"`
}

func ProjectConfidential(requested string, node Attestation) ConfidentialStatus {
	if requested == "" || requested == "none" {
		return ConfidentialStatus{Reason: "not requested"}
	}
	if err := AdmitConfidential(requested, node); err != nil {
		return ConfidentialStatus{Kind: requested, Sealed: false, Reason: err.Error()}
	}
	return ConfidentialStatus{Kind: requested, Sealed: true}
}
