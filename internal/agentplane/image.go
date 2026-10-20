// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agentplane

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// AdmitImage requires a sha256 digest always when an agent pool is
// set, and a cosign signature annotation of the form
// cosign:sha256:<64 hex>. Signature verification stays with the
// caller; this function checks the admission shape.
func AdmitImage(agentPool bool, digest, signature string) error {
	if !agentPool {
		return nil
	}
	if !strings.HasPrefix(digest, "sha256:") || !isHex64(strings.TrimPrefix(digest, "sha256:")) {
		return fmt.Errorf("agent pool image digest must be sha256:<64 hex>")
	}
	const prefix = "cosign:sha256:"
	if !strings.HasPrefix(signature, prefix) || !isHex64(strings.TrimPrefix(signature, prefix)) {
		return fmt.Errorf("agent pool image signature must be %s<64 hex>", prefix)
	}
	return nil
}

func isHex64(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}
