// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMachineDecodesOldShapeJSON proves a Machine JSON blob written by an
// older Kairon version -- one that predates fields like guestAgent,
// serviceFabric, instanceTypeName, security, and deviceClaims -- still
// decodes cleanly into today's Machine struct, with every newer field
// simply zero-valued. This is the "upgrade" half of Kairon's CRD story:
// every field this project has ever added to spec/status has been a pure,
// optional addition (see docs/guides/crd-versioning.md), so a rolling
// upgrade must never choke on an object a previous version wrote.
func TestMachineDecodesOldShapeJSON(t *testing.T) {
	const oldShape = `{
		"apiVersion": "kairon.zyvor.dev/v1",
		"kind": "Machine",
		"metadata": {"name": "db", "namespace": "prod"},
		"spec": {
			"nodeName": "worker-1",
			"image": {"path": "/images/db.qcow2"},
			"resources": {"cpu": "2", "memory": "2Gi"},
			"powerState": "Running"
		},
		"status": {
			"nodeName": "worker-1",
			"phase": "Running",
			"runtimeID": "vm-1"
		}
	}`
	var m Machine
	if err := json.Unmarshal([]byte(oldShape), &m); err != nil {
		t.Fatalf("old-shape Machine JSON failed to decode: %v", err)
	}
	if m.Spec.NodeName != "worker-1" || m.Spec.Image.Path != "/images/db.qcow2" {
		t.Fatalf("known fields didn't survive decode: %+v", m.Spec)
	}
	if m.Spec.GuestAgent.Enabled || m.Spec.InstanceTypeName != "" || len(m.Spec.DeviceClaims) != 0 {
		t.Fatalf("expected every field added after this shape to be zero-valued, got %+v", m.Spec)
	}
	if m.Status.Phase != "Running" {
		t.Fatalf("status didn't survive decode: %+v", m.Status)
	}
}

// TestMachineToleratesUnknownFutureFields proves the opposite direction: a
// Machine JSON blob carrying fields this binary doesn't know about yet
// (the shape a *newer* Kairon version, or a real kairon.zyvor.dev/v1
// conversion, might one day write) still decodes without error and without
// losing any field this binary *does* know about -- Go's encoding/json
// silently ignores unrecognized object keys by default, but this pins that
// behavior down as a real, explicit guarantee rather than an unverified
// assumption. This is what lets an old kairon-node/kairon-controller binary
// coexist during a rolling upgrade, the same real scenario
// internal/agent/agent.go's own ListMachineMigrations 404-tolerance comment
// already names for a not-yet-installed CRD.
func TestMachineToleratesUnknownFutureFields(t *testing.T) {
	const futureShape = `{
		"apiVersion": "kairon.zyvor.dev/v1",
		"kind": "Machine",
		"metadata": {"name": "db", "namespace": "prod", "futureMetadataField": "ignored"},
		"spec": {
			"nodeName": "worker-1",
			"image": {"path": "/images/db.qcow2"},
			"resources": {"cpu": "2", "memory": "2Gi"},
			"confidentialComputing": {"technology": "sev-snp"},
			"someBrandNewField": {"nested": [1, 2, 3]}
		},
		"status": {
			"phase": "Running",
			"someFutureStatusField": true
		}
	}`
	var m Machine
	if err := json.Unmarshal([]byte(futureShape), &m); err != nil {
		t.Fatalf("future-shape Machine JSON failed to decode: %v", err)
	}
	if m.Spec.NodeName != "worker-1" || m.Spec.Image.Path != "/images/db.qcow2" {
		t.Fatalf("known fields didn't survive decode alongside unknown ones: %+v", m.Spec)
	}
	if m.Status.Phase != "Running" {
		t.Fatalf("known status fields didn't survive decode: %+v", m.Status)
	}

	// Round-tripping back to JSON must never leak the unknown fields into
	// this binary's own writes -- there's nowhere for them to be held.
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if strings.Contains(string(out), "someBrandNewField") || strings.Contains(string(out), "confidentialComputing") {
		t.Fatalf("expected unknown fields to be dropped on round-trip, got %s", out)
	}
}

// TestStatusMessageFieldsAlwaysSerialize is a direct regression test for a
// bug internal/integration's own failure-injection test
// (TestAgentRecoversFromTransientFluxVMOutage) originally caught: a status
// struct's Message field marshaled with `omitempty` gets dropped from the
// JSON entirely once it's back to "", and every status update in this
// project is sent as one whole-object JSON *merge patch*
// (internal/kube.Client.Patch*Status) -- under RFC 7386 semantics, a
// missing key means "leave this alone," not "clear it." Marshaling a fresh
// zero-value status must always include an explicit `"message":""` so a
// reconcile that recovers from an earlier error can actually clear it,
// rather than leaving the stale error message in status forever.
func TestStatusMessageFieldsAlwaysSerialize(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"MachineStatus", MachineStatus{}},
		{"MachineMigrationStatus", MachineMigrationStatus{}},
		{"MachineSnapshotStatus", MachineSnapshotStatus{}},
		{"MachineSetStatus", MachineSetStatus{}},
		{"MachineSnapshotRestoreStatus", MachineSnapshotRestoreStatus{}},
		{"MachineNetworkPolicyStatus", MachineNetworkPolicyStatus{}},
		{"NetworkSecurityGroupStatus", NetworkSecurityGroupStatus{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := json.Marshal(c.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(out), `"message":""`) {
				t.Fatalf("expected an explicit empty message key so a merge-patch can clear a stale one, got %s", out)
			}
		})
	}
}
