// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The cloud metadata endpoint must never be reachable through an image URL.
func TestDownloadToTempRefusesLinkLocal(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "img")
	err := downloadToTemp(context.Background(), "http://169.254.169.254/latest/meta-data/", dst, strings.Repeat("0", 64), false, 0)
	if err == nil || !strings.Contains(err.Error(), "link-local") {
		t.Fatalf("expected a link-local refusal, got %v", err)
	}
}
