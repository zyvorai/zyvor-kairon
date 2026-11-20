// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestOfflineMachineChecks(t *testing.T) {
	good := model.Machine{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}}}
	if got := OfflineMachineChecks(good); len(got) != 0 {
		t.Fatalf("good Machine: %v", got)
	}
	bad := good
	bad.Spec.Resources.CPU = "NaN"
	bad.Spec.Network.MAC = "zz"
	bad.Spec.Disks = []model.MachineDisk{{Name: "root", ClaimName: "c"}}
	got := OfflineMachineChecks(bad)
	rules := map[string]bool{}
	for _, c := range got {
		rules[c.Rule] = true
	}
	for _, r := range []string{"resource-quantities", "network-mac", "disks"} {
		if !rules[r] {
			t.Errorf("missing rule %s in %v", r, got)
		}
	}
	// The webhook's own helper must agree with the exported one.
	if validateResourceQuantities(bad.Spec.Resources) == nil {
		t.Error("webhook accepts NaN cpu")
	}
}
