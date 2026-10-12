// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

func facts(flags ...string) HibernationHostFacts {
	return HibernationHostFacts{QEMUVersion: "10.2.1", MachineType: "pc-q35-10.2", CPUVendor: "GenuineIntel", CPUFlags: flags, StateDir: "/mnt/shared/hibernation"}
}

func TestHibernationCompatibleSupersetFlags(t *testing.T) {
	if r := HibernationIncompatibilities(facts("sse4_2"), facts("sse4_2", "avx512f")); len(r) != 0 {
		t.Fatalf("superset host should be compatible: %v", r)
	}
}

func TestHibernationIncompatibilities(t *testing.T) {
	host := facts("sse4_2")
	host.QEMUVersion, host.MachineType, host.CPUVendor, host.StateDir = "9.2.0", "pc-q35-9.2", "AuthenticAMD", "/var/lib/fluxvm/hibernation"
	r := HibernationIncompatibilities(facts("sse4_2", "avx512f"), host)
	if len(r) != 5 {
		t.Fatalf("want 5 reasons, got %v", r)
	}
	if !strings.Contains(r[3], "avx512f") || !strings.Contains(r[4], "hibernation directory") {
		t.Fatalf("unexpected reasons %v", r)
	}
}

func TestPowerStateFreesCapacity(t *testing.T) {
	for p, want := range map[string]bool{"Running": false, "Paused": false, "Stopped": true, "Halted": true, PowerStateHibernated: true} {
		if got := PowerStateFreesCapacity(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}
