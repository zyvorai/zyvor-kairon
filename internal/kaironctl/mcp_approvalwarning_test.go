// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"strings"
	"testing"
)

func TestMCPApprovalModeWarning(t *testing.T) {
	if w := mcpApprovalModeWarning("resource"); w != "" {
		t.Fatalf("resource mode must not warn, got %q", w)
	}
	for _, mode := range []string{"", "annotation"} {
		w := mcpApprovalModeWarning(mode)
		if !strings.Contains(w, "KAIRON_MCP_APPROVAL_MODE=resource") {
			t.Errorf("mode %q: warning should name the strict mode, got %q", mode, w)
		}
	}
}
