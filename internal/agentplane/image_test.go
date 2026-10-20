// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agentplane

import (
	"strings"
	"testing"
)

func TestAdmitImageRequiresHexDigestAndSignature(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	nonHex := strings.Repeat("zz", 32)
	if err := AdmitImage(false, "junk", ""); err != nil {
		t.Fatalf("non-agent pool must pass: %v", err)
	}
	if err := AdmitImage(true, "sha256:"+hex64, "cosign:sha256:"+hex64); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if AdmitImage(true, "sha256:"+nonHex, "cosign:sha256:"+hex64) == nil {
		t.Fatal("non-hex digest admitted")
	}
	if AdmitImage(true, "sha256:"+hex64, "cosign:sha256:"+nonHex) == nil {
		t.Fatal("non-hex signature admitted")
	}
	if AdmitImage(true, "sha256:"+hex64[:62], "cosign:sha256:"+hex64) == nil {
		t.Fatal("short digest admitted")
	}
}
