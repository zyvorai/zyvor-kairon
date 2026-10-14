// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package macnode

import (
	"strings"
	"testing"
)

func TestMemTotalMiBReadsProcMeminfo(t *testing.T) {
	in := "MemTotal:       32768000 kB\nMemFree:         1024000 kB\n"
	if got := memTotalMiB(strings.NewReader(in)); got != 32000 {
		t.Fatalf("memTotalMiB = %d, want 32000", got)
	}
	if got := memTotalMiB(strings.NewReader("MemFree: 1 kB\n")); got != 0 {
		t.Fatalf("no MemTotal line: got %d, want 0", got)
	}
}
