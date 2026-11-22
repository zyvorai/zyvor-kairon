// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import "testing"

func TestParseIntOrPercentPlainInteger(t *testing.T) {
	got, err := ParseIntOrPercent("3", 10)
	if err != nil || got != 3 {
		t.Fatalf("got %d err=%v", got, err)
	}
}

func TestParseIntOrPercentRoundsUp(t *testing.T) {
	got, err := ParseIntOrPercent("50%", 5)
	if err != nil || got != 3 {
		t.Fatalf("got %d err=%v, want 3 (ceil(2.5))", got, err)
	}
}

func TestParseIntOrPercentRejectsInvalid(t *testing.T) {
	for _, v := range []string{"", "abc", "-1", "150%", "-5%"} {
		if _, err := ParseIntOrPercent(v, 10); err == nil {
			t.Fatalf("expected an error for %q", v)
		}
	}
}

func TestDesiredHealthyFromMinAvailable(t *testing.T) {
	spec := MachineDisruptionBudgetSpec{MinAvailable: "2"}
	got, err := spec.DesiredHealthy(5)
	if err != nil || got != 2 {
		t.Fatalf("got %d err=%v", got, err)
	}
}

func TestDesiredHealthyFromMaxUnavailable(t *testing.T) {
	spec := MachineDisruptionBudgetSpec{MaxUnavailable: "1"}
	got, err := spec.DesiredHealthy(5)
	if err != nil || got != 4 {
		t.Fatalf("got %d err=%v, want 4 (5-1)", got, err)
	}
}

func TestDesiredHealthyRejectsBothOrNeitherSet(t *testing.T) {
	if _, err := (MachineDisruptionBudgetSpec{MinAvailable: "1", MaxUnavailable: "1"}).DesiredHealthy(5); err == nil {
		t.Fatal("expected an error when both are set")
	}
	if _, err := (MachineDisruptionBudgetSpec{}).DesiredHealthy(5); err == nil {
		t.Fatal("expected an error when neither is set")
	}
}
