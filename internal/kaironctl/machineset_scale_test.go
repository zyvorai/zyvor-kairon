// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
package kaironctl

import "testing"

func TestParseReplicas(t *testing.T) {
	n, err := ParseReplicas("3")
	if err != nil || n != 3 {
		t.Fatalf("got %d %v", n, err)
	}
	if _, err := ParseReplicas("-1"); err == nil {
		t.Fatal("expected error for negative")
	}
	if _, err := ParseReplicas("abc"); err == nil {
		t.Fatal("expected error for non-numeric")
	}
}

func TestScaleCommand(t *testing.T) {
	got := ScaleCommand("web", 5)
	want := "kubectl scale machineset web --replicas=5"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
