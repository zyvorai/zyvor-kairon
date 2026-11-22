// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package controller

import (
	"fmt"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	// StorageDomainLabel and NetworkDomainLabel are opt-in node labels an
	// operator sets to tell migrationPreflight which nodes actually share
	// storage/network reachability -- Kairon has no other way to know
	// this (see internal/model.Node's doc comment: no storage/network
	// capability fields exist on a Node, only generic labels/addresses/
	// conditions, the same data spec.placement.nodeSelector/affinity
	// already work with). Two nodes with the SAME value for one of these
	// labels are asserted, by the operator, to be compatible; an empty or
	// absent value on either side means "can't confirm either way," not
	// "incompatible."
	StorageDomainLabel = "kairon.zyvor.dev/storage-domain"
	NetworkDomainLabel = "kairon.zyvor.dev/network-domain"

	// VFIODevicesLabel is an opt-in, operator-asserted node label naming
	// the PCI BDFs (or vendor:device IDs) a node's VFIO allowlist
	// (KAIRON_VFIO_ALLOWLIST) actually permits -- see
	// deviceClaimsPreflight's own doc comment for why this exists.
	// Comma-separated, same shape as KAIRON_VFIO_ALLOWLIST itself.
	VFIODevicesLabel = "kairon.zyvor.dev/vfio-devices"
)

// migrationPreflight closes part of the "storage/network migration
// preflight" gap: docs/runbook-multi-host-migration-test.md's own
// Prerequisites table documents that both hosts need "a shared filesystem
// mount at an identical path" and "L2/L3 network reachability" for a
// migration to actually work -- today that's pure, unchecked operator
// discipline (Kairon neither copies disk content nor guest network state
// between nodes in either cold or live migration, see
// docs/architecture.md's Cold/Live migration sections). This can only
// catch a *confirmed* mismatch (both nodes opted in with StorageDomainLabel/
// NetworkDomainLabel and they disagree) -- it cannot prove compatibility
// when the labels aren't set, since Kairon has no other source of truth
// for node storage/network topology. That's a real, current limit, not
// an oversight: see docs/guides/machine-fencing.md.
//
// Checked against the chosen migration target only, not searched across
// every candidate -- a first cut. If the winning candidate fails
// preflight, the migration is blocked outright rather than silently
// retrying a different node.
func migrationPreflight(source, target model.Node) string {
	if reason := domainMismatch(StorageDomainLabel, "storage", source, target); reason != "" {
		return reason
	}
	if reason := domainMismatch(NetworkDomainLabel, "network", source, target); reason != "" {
		return reason
	}
	return ""
}

// deviceClaimsPreflight refuses migrating a Machine with spec.deviceClaims
// set, unless it's a cold migration to a target asserted (via
// VFIODevicesLabel) to have an equivalent device. This is stricter than
// domainMismatch's "can't confirm either way, so allow it" posture,
// deliberately: VFIO passthrough is boot-time-only (internal/agent's
// resolveVFIODevices runs once, at CreateWithVFIO -- see
// internal/fluxvm.Client), and QEMU's live-migration RAM/state transfer
// (internal/migration) fundamentally cannot carry a passthrough PCI
// device's in-flight state across hosts -- there is no FluxVM API to
// hot-unplug/hot-plug one around a migration today (compare
// HotplugCPU/HotplugMemory, which do exist), so a *live* migration of a
// deviceClaims Machine can never work, label or no label, until FluxVM
// gains that capability (tracked in ROADMAP.md). A *cold* migration has
// no such problem -- the guest is stopped and a fresh runtime is created
// at the target exactly like initial creation, so it only needs the
// target to actually have a compatible device, which is exactly what
// VFIODevicesLabel lets an operator assert (Kairon has no other source of
// truth for a node's VFIO allowlist, the same limitation
// StorageDomainLabel/NetworkDomainLabel already document).
func deviceClaimsPreflight(machine model.Machine, target model.Node, strategy string) string {
	if len(machine.Spec.DeviceClaims) == 0 {
		return ""
	}
	if strategy == "live" {
		return "Machine has spec.deviceClaims set -- live migration cannot carry a VFIO passthrough device's state across hosts (FluxVM has no hot-unplug/hot-plug API for this yet, see ROADMAP.md); use strategy: cold instead"
	}
	if strings.TrimSpace(target.Metadata.Labels[VFIODevicesLabel]) == "" {
		return fmt.Sprintf("Machine has spec.deviceClaims set, and target node %q is not asserted (via %s) to have an equivalent device", target.Metadata.Name, VFIODevicesLabel)
	}
	return ""
}

func domainMismatch(label, kind string, source, target model.Node) string {
	sv, sok := source.Metadata.Labels[label]
	tv, tok := target.Metadata.Labels[label]
	if !sok || !tok || sv == "" || tv == "" {
		return "" // can't confirm compatibility either way -- not a new hard requirement
	}
	if sv != tv {
		return fmt.Sprintf("%s domain mismatch: source node %q has %s=%q, target node %q has %s=%q -- these nodes are not asserted to share storage/network, see docs/guides/machine-fencing.md", kind, source.Metadata.Name, label, sv, target.Metadata.Name, label, tv)
	}
	return ""
}
