// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestQuantitiesRejectNonFiniteAndOverflow(t *testing.T) {
	for _, v := range []string{"NaN", "Inf", "+Inf", "1e30", "5000000000", "NaNm", "1e40m"} {
		if n, err := ParseVCPUs(v); err == nil {
			t.Errorf("ParseVCPUs(%q) = %d, want error", v, n)
		}
	}
	for _, v := range []string{"NaN", "Inf", "NaNMi", "InfGi", "1e30Gi", "1e30"} {
		if n, err := ParseMemoryMiB(v); err == nil {
			t.Errorf("ParseMemoryMiB(%q) = %d, want error", v, n)
		}
	}
	if _, err := ParseBytes("NaN"); err == nil {
		t.Error("ParseBytes(NaN) accepted")
	}
	if n, err := ParseVCPUs("2"); err != nil || n != 2 {
		t.Errorf("ParseVCPUs(2) = %d, %v", n, err)
	}
}

func TestAddFinalizerMetaGuardsWithResourceVersion(t *testing.T) {
	orig := []string{"a"}
	meta := AddFinalizerMeta(orig, "b", "42")
	if got := meta["finalizers"].([]string); len(got) != 2 || got[1] != "b" || len(orig) != 1 {
		t.Fatalf("finalizers=%v orig=%v", got, orig)
	}
	if meta["resourceVersion"] != "42" {
		t.Fatalf("a merge patch that replaces the finalizer list must carry resourceVersion, got %v", meta)
	}
	if _, ok := AddFinalizerMeta(nil, "b", "")["resourceVersion"]; ok {
		t.Fatal("empty resourceVersion must be omitted")
	}
}
