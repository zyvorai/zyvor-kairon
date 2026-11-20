// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func nodeWithLabels(name string, labels map[string]string) model.Node {
	n := readyCapableNode(name)
	for k, v := range labels {
		n.Metadata.Labels[k] = v
	}
	return n
}

func TestMigrationPreflightAllowsMatchingDomains(t *testing.T) {
	source := nodeWithLabels("worker-1", map[string]string{StorageDomainLabel: "rack-a", NetworkDomainLabel: "vlan-10"})
	target := nodeWithLabels("worker-2", map[string]string{StorageDomainLabel: "rack-a", NetworkDomainLabel: "vlan-10"})
	if blocker := migrationPreflight(source, target); blocker != "" {
		t.Fatalf("got blocker %q, want none for matching domains", blocker)
	}
}

func TestMigrationPreflightBlocksStorageDomainMismatch(t *testing.T) {
	source := nodeWithLabels("worker-1", map[string]string{StorageDomainLabel: "rack-a"})
	target := nodeWithLabels("worker-2", map[string]string{StorageDomainLabel: "rack-b"})
	blocker := migrationPreflight(source, target)
	if blocker == "" {
		t.Fatal("expected a blocker for mismatched storage domains")
	}
}

func TestMigrationPreflightBlocksNetworkDomainMismatch(t *testing.T) {
	source := nodeWithLabels("worker-1", map[string]string{NetworkDomainLabel: "vlan-10"})
	target := nodeWithLabels("worker-2", map[string]string{NetworkDomainLabel: "vlan-20"})
	blocker := migrationPreflight(source, target)
	if blocker == "" {
		t.Fatal("expected a blocker for mismatched network domains")
	}
}

func TestMigrationPreflightAllowsUnsetLabels(t *testing.T) {
	source := readyCapableNode("worker-1")
	target := readyCapableNode("worker-2")
	if blocker := migrationPreflight(source, target); blocker != "" {
		t.Fatalf("got blocker %q, want none when neither node opted in with domain labels", blocker)
	}
}

func TestMigrationPreflightAllowsOneSidedLabel(t *testing.T) {
	source := nodeWithLabels("worker-1", map[string]string{StorageDomainLabel: "rack-a"})
	target := readyCapableNode("worker-2")
	if blocker := migrationPreflight(source, target); blocker != "" {
		t.Fatalf("got blocker %q, want none when only one node opted in (can't confirm either way)", blocker)
	}
}

func TestDomainMismatchIgnoresEmptyLabelValue(t *testing.T) {
	source := nodeWithLabels("worker-1", map[string]string{StorageDomainLabel: ""})
	target := nodeWithLabels("worker-2", map[string]string{StorageDomainLabel: "rack-b"})
	if reason := domainMismatch(StorageDomainLabel, "storage", source, target); reason != "" {
		t.Fatalf("got reason %q, want none for an empty label value", reason)
	}
}

func machineWithDeviceClaim() model.Machine {
	return model.Machine{Spec: model.MachineSpec{DeviceClaims: []model.DeviceClaimReference{{Name: "gpu-0"}}}}
}

func TestDeviceClaimsPreflightAllowsMachineWithoutDeviceClaims(t *testing.T) {
	target := readyCapableNode("worker-2")
	if blocker := deviceClaimsPreflight(model.Machine{}, target, "live"); blocker != "" {
		t.Fatalf("got blocker %q, want none for a Machine with no deviceClaims", blocker)
	}
}

func TestDeviceClaimsPreflightBlocksLiveMigrationUnconditionally(t *testing.T) {
	target := nodeWithLabels("worker-2", map[string]string{VFIODevicesLabel: "0000:65:00.0"})
	if blocker := deviceClaimsPreflight(machineWithDeviceClaim(), target, "live"); blocker == "" {
		t.Fatal("expected live migration of a deviceClaims Machine to always be blocked, even with a matching target label")
	}
}

func TestDeviceClaimsPreflightBlocksColdMigrationWithoutTargetLabel(t *testing.T) {
	target := readyCapableNode("worker-2")
	if blocker := deviceClaimsPreflight(machineWithDeviceClaim(), target, "cold"); blocker == "" {
		t.Fatal("expected cold migration of a deviceClaims Machine to be blocked when the target has no VFIODevicesLabel")
	}
}

func TestDeviceClaimsPreflightAllowsColdMigrationWithTargetLabel(t *testing.T) {
	target := nodeWithLabels("worker-2", map[string]string{VFIODevicesLabel: "0000:65:00.0"})
	if blocker := deviceClaimsPreflight(machineWithDeviceClaim(), target, "cold"); blocker != "" {
		t.Fatalf("got blocker %q, want none for cold migration to a target asserting an equivalent device", blocker)
	}
}
