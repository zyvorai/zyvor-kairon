// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func newTestControllerServer(t *testing.T, run CommandRunner) *ControllerServer {
	t.Helper()
	s, err := NewControllerServer(run, "10.0.0.5:3260", t.TempDir())
	if err != nil {
		t.Fatalf("NewControllerServer: %v", err)
	}
	return s
}

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}
}

// createSparseFile creates a sparse file reporting sizeBytes via Stat
// without actually writing sizeBytes of data -- these tests only care
// about the size ControllerExpandVolume's own os.Stat call observes.
func createSparseFile(path string, sizeBytes int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Truncate(sizeBytes)
}

func TestNewControllerServerValidatesPortalAndCreatesVolumeDir(t *testing.T) {
	if _, err := NewControllerServer(newFakeCommandRunner(), "", t.TempDir()); err == nil {
		t.Fatal("expected an error for an empty portal")
	}
	if _, err := NewControllerServer(newFakeCommandRunner(), "not-a-host-port", t.TempDir()); err == nil {
		t.Fatal("expected an error for a portal that isn't host:port")
	}
	if _, err := NewControllerServer(newFakeCommandRunner(), "10.0.0.5:3260", ""); err == nil {
		t.Fatal("expected an error for an empty volumeDir")
	}

	dir := filepath.Join(t.TempDir(), "nested", "volumes")
	if _, err := NewControllerServer(newFakeCommandRunner(), "10.0.0.5:3260", dir); err != nil {
		t.Fatalf("NewControllerServer: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("expected volumeDir to be created, stat: %v", err)
	}
}

func TestControllerGetCapabilitiesReportsAllSupportedRPCs(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	resp, err := s.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	want := map[csi.ControllerServiceCapability_RPC_Type]bool{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME:   false,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME:          false,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT: false,
	}
	if len(resp.Capabilities) != len(want) {
		t.Fatalf("expected exactly %d capabilities, got %+v", len(want), resp.Capabilities)
	}
	for _, c := range resp.Capabilities {
		typ := c.GetRpc().GetType()
		if _, ok := want[typ]; !ok {
			t.Fatalf("unexpected capability %v", typ)
		}
		want[typ] = true
	}
	for typ, seen := range want {
		if !seen {
			t.Fatalf("missing expected capability %v", typ)
		}
	}
}

func TestCreateVolumeRequiresNameAndCapabilities(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}}); err == nil {
		t.Fatal("expected an error for an empty name")
	}
	if _, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-abc123"}); err == nil {
		t.Fatal("expected an error for missing volume_capabilities")
	}
}

func TestCreateVolumeRejectsInvalidName(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	req := &csi.CreateVolumeRequest{Name: "Not_Valid!", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}}
	if _, err := s.CreateVolume(context.Background(), req); err == nil {
		t.Fatal("expected an error for a name that isn't a valid filename/IQN component")
	}
}

func TestCreateVolumeAcceptsBlockOnlyCapability(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	req := &csi.CreateVolumeRequest{
		Name: "pvc-abc123",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		}},
	}
	if _, err := s.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("raw block mode: %v", err)
	}
}

func TestCreateVolumeDefaultsSizeWhenNoCapacityRange(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	req := &csi.CreateVolumeRequest{Name: "pvc-abc123", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}}
	resp, err := s.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != defaultVolumeSizeBytes {
		t.Fatalf("expected the default size %d, got %d", int64(defaultVolumeSizeBytes), resp.Volume.CapacityBytes)
	}
}

// TestCreateVolumeReturnsAConsumableVolumeIDAndContext confirms the
// output of a real CreateVolume call is exactly what NodeServer's own
// parseISCSIConfig/decodeVolumeID already know how to consume -- a
// dynamically provisioned volume must be indistinguishable from a
// statically hand-written one by the time it reaches NodeStageVolume.
func TestCreateVolumeReturnsAConsumableVolumeIDAndContext(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	req := &csi.CreateVolumeRequest{
		Name:               "pvc-abc123",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 5 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	}
	resp, err := s.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if resp.Volume.CapacityBytes != 5<<30 {
		t.Fatalf("expected CapacityBytes to echo the request, got %d", resp.Volume.CapacityBytes)
	}

	wantIQN := "iqn.2026-01.dev.zyvor.kairon:pvc-abc123"
	vc := resp.Volume.VolumeContext
	if vc[volumeAttrPortal] != "10.0.0.5:3260" || vc[volumeAttrIQN] != wantIQN || vc[volumeAttrLUN] != defaultLUN {
		t.Fatalf("unexpected volume_context: %+v", vc)
	}

	cfg, err := parseISCSIConfig(vc, nil)
	if err != nil {
		t.Fatalf("NodeServer's own parseISCSIConfig rejected the returned volume_context: %v", err)
	}
	if cfg.Portal != "10.0.0.5:3260" || cfg.IQN != wantIQN {
		t.Fatalf("parsed config didn't match: %+v", cfg)
	}

	decoded, err := decodeVolumeID(resp.Volume.VolumeId)
	if err != nil {
		t.Fatalf("NodeServer's own decodeVolumeID rejected the returned volume_id: %v", err)
	}
	if decoded.IQN != wantIQN || decoded.LUN != defaultLUN {
		t.Fatalf("decoded volume_id didn't match: %+v", decoded)
	}
}

// TestCreateVolumeIsIdempotentOnRetry mirrors the CSI spec's own
// requirement: calling CreateVolume twice with the same Name (the
// external-provisioner sidecar's retry behavior on a transient failure)
// must succeed both times, not fail the second time because the target
// already exists.
func TestCreateVolumeIsIdempotentOnRetry(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	req := &csi.CreateVolumeRequest{Name: "pvc-retry", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}}
	if _, err := s.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	if _, err := s.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("retried CreateVolume: %v", err)
	}
}

func TestDeleteVolumeRequiresVolumeID(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{}); err == nil {
		t.Fatal("expected an error for an empty volume_id")
	}
}

func TestDeleteVolumeIsIdempotentAgainstUnknownVolumeID(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	if _, err := s.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "not-a-kairon-volume-id"}); err != nil {
		t.Fatalf("expected an unrecognized volume_id to be treated as already-deleted, got %v", err)
	}
	if calls := run.callsFor("targetcli"); len(calls) != 0 {
		t.Fatalf("expected no targetcli calls for an unrecognized volume_id, got %d", len(calls))
	}
}

// TestDeleteVolumeIsIdempotentAgainstStaticVolumeID confirms DeleteVolume
// never touches a statically-provisioned volume it didn't create --
// docs/guides/machine-storage-csi.md's own example IQNs never carry
// iqnPrefix, so nameFromIQN correctly refuses to treat them as this
// Controller's to delete.
func TestDeleteVolumeIsIdempotentAgainstStaticVolumeID(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	staticID := encodeVolumeID(iscsiConfig{Portal: "192.168.1.50:3260", IQN: "iqn.2026-01.example:statictarget", LUN: "0"})
	if _, err := s.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: staticID}); err != nil {
		t.Fatalf("expected a static volume_id to be treated as already-deleted, got %v", err)
	}
	if calls := run.callsFor("targetcli"); len(calls) != 0 {
		t.Fatalf("expected no targetcli calls for a statically-provisioned volume_id, got %d", len(calls))
	}
}

func TestDeleteVolumeDeletesTargetBackstoreAndBackingFile(t *testing.T) {
	run := newFakeCommandRunner()
	dir := t.TempDir()
	s, err := NewControllerServer(run, "10.0.0.5:3260", dir)
	if err != nil {
		t.Fatalf("NewControllerServer: %v", err)
	}

	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-todelete", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	backingFile := s.backingFilePath("pvc-todelete")
	if err := os.WriteFile(backingFile, []byte("fake image"), 0o600); err != nil {
		t.Fatalf("write fake backing file: %v", err)
	}

	if _, err := s.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: created.Volume.VolumeId}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	deleteTargetCalls := 0
	deleteBackstoreCalls := 0
	for _, c := range run.callsFor("targetcli") {
		joined := joinArgs(c[1:])
		if len(c) >= 3 && c[1] == "/iscsi" && c[2] == "delete" {
			deleteTargetCalls++
		}
		if len(c) >= 3 && c[1] == "/backstores/fileio" && c[2] == "delete" {
			deleteBackstoreCalls++
		}
		_ = joined
	}
	if deleteTargetCalls != 1 {
		t.Fatalf("expected exactly 1 target delete call, got %d", deleteTargetCalls)
	}
	if deleteBackstoreCalls != 1 {
		t.Fatalf("expected exactly 1 backstore delete call, got %d", deleteBackstoreCalls)
	}
	if _, err := os.Stat(backingFile); !os.IsNotExist(err) {
		t.Fatalf("expected the backing file to be removed, stat error: %v", err)
	}
}

func TestDeleteVolumeToleratesAlreadyRemovedBackingFile(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-nofile", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// No backing file was ever written to disk in this test -- DeleteVolume
	// must still succeed (os.Remove's IsNotExist tolerance).
	if _, err := s.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: created.Volume.VolumeId}); err != nil {
		t.Fatalf("expected DeleteVolume to tolerate an already-missing backing file, got %v", err)
	}
}

// TestCreateVolumeWithoutSecretsStaysInDemoMode asserts that omitting a
// StorageClass provisioner secret (the default, and the only option a
// Kairon Machine's own boot-disk path can ever use -- see
// docs/guides/machine-storage-csi.md) doesn't attempt any CHAP
// configuration.
func TestCreateVolumeWithoutSecretsStaysInDemoMode(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	if _, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-nochap", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	for _, c := range run.callsFor("targetcli") {
		if len(c) >= 4 && c[2] == "set" && c[3] == "auth" {
			t.Fatalf("expected no CHAP configuration without a provisioner secret, got %q", joinArgs(c[1:]))
		}
	}
}

// TestCreateVolumeWithProvisionerSecretConfiguresCHAP mirrors a
// StorageClass whose csi.storage.k8s.io/provisioner-secret-name points at
// a Secret carrying username/password -- the external-provisioner
// sidecar resolves it and passes it through as req.Secrets.
func TestCreateVolumeWithProvisionerSecretConfiguresCHAP(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	if _, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-chap",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		Secrets:            map[string]string{secretKeyUsername: "alice", secretKeyPassword: "s3cret"},
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	var sawAuth bool
	for _, c := range run.callsFor("targetcli") {
		if len(c) >= 4 && c[2] == "set" && c[3] == "auth" {
			sawAuth = true
			if got := joinArgs(c[1:]); got != "/iscsi/iqn.2026-01.dev.zyvor.kairon:pvc-chap/tpg1 set auth userid=alice password=s3cret" {
				t.Fatalf("unexpected auth command: %q", got)
			}
		}
	}
	if !sawAuth {
		t.Fatal("expected a CHAP auth command when a provisioner secret is set")
	}
}

func TestCreateVolumeRejectsHalfConfiguredProvisionerSecret(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	_, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-halfchap",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		Secrets:            map[string]string{secretKeyUsername: "alice"},
	})
	if err == nil {
		t.Fatal("expected an error when only username is set")
	}
}

func TestControllerExpandVolumeRequiresVolumeIDAndSize(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{}); err == nil {
		t.Fatal("expected an error for an empty volume_id")
	}
	if _, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "iscsi|10.0.0.5:3260|iqn.2026-01.dev.zyvor.kairon:pvc-x|0",
	}); err == nil {
		t.Fatal("expected an error for a missing capacity_range")
	}
}

func TestControllerExpandVolumeGrowsBackstore(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-grow", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := createSparseFile(s.backingFilePath("pvc-grow"), 1<<30); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}

	resp, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      created.Volume.VolumeId,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if !resp.NodeExpansionRequired {
		t.Fatal("expected NodeExpansionRequired to be set")
	}
	if resp.CapacityBytes != 2<<30 {
		t.Fatalf("expected CapacityBytes 2<<30, got %d", resp.CapacityBytes)
	}
	var sawResize bool
	for _, c := range run.callsFor("targetcli") {
		if got := joinArgs(c[1:]); got == "/backstores/fileio/pvc-grow resize 2147483648" {
			sawResize = true
		}
	}
	if !sawResize {
		t.Fatal("expected a backstore resize command")
	}
}

func TestControllerExpandVolumeIsIdempotentWhenAlreadyBigEnough(t *testing.T) {
	run := newFakeCommandRunner()
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-noop", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := createSparseFile(s.backingFilePath("pvc-noop"), 2<<30); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}

	before := len(run.callsFor("targetcli"))
	if _, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      created.Volume.VolumeId,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if after := len(run.callsFor("targetcli")); after != before {
		t.Fatalf("expected no new targetcli calls for an already-big-enough volume, got %d new", after-before)
	}
}

func TestControllerExpandVolumeRejectsUnknownVolume(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	_, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "iscsi|10.0.0.5:3260|iqn.2026-01.dev.zyvor:static-disk|0",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err == nil {
		t.Fatal("expected an error for a volume this Controller never dynamically provisioned")
	}
}

// fakeCPHandler stands in for the real `cp --reflink=auto <src> <dst>`
// this driver shells out to (lioClient.copyFile) -- the fake
// CommandRunner otherwise just no-ops without touching the filesystem,
// which would make every os.Stat these snapshot tests rely on fail.
func fakeCPHandler(args ...string) (string, error) {
	if len(args) != 3 || args[0] != "--reflink=auto" {
		return "", fmt.Errorf("unexpected cp args: %v", args)
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(args[2]), 0o700); err != nil {
		return "", err
	}
	return "", os.WriteFile(args[2], data, 0o600)
}

func TestCreateSnapshotRequiresNameAndSourceVolumeID(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{SourceVolumeId: "x"}); err == nil {
		t.Fatal("expected an error for an empty name")
	}
	if _, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-1"}); err == nil {
		t.Fatal("expected an error for an empty source_volume_id")
	}
}

func TestCreateSnapshotRejectsUnknownSourceVolume(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	_, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snap-1", SourceVolumeId: "iscsi|10.0.0.5:3260|iqn.2026-01.dev.zyvor:static-disk|0",
	})
	if err == nil {
		t.Fatal("expected an error for a volume this Controller never dynamically provisioned")
	}
}

func TestCreateSnapshotCopiesSourceVolumeBackingFile(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("cp", fakeCPHandler)
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-src", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	content := []byte("real volume content")
	if err := os.WriteFile(s.backingFilePath("pvc-src"), content, 0o600); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}

	resp, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snap-1", SourceVolumeId: created.Volume.VolumeId,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if resp.Snapshot.SnapshotId != "snap-1" || resp.Snapshot.SourceVolumeId != created.Volume.VolumeId {
		t.Fatalf("unexpected snapshot: %+v", resp.Snapshot)
	}
	if resp.Snapshot.SizeBytes != int64(len(content)) || !resp.Snapshot.ReadyToUse {
		t.Fatalf("unexpected snapshot metadata: %+v", resp.Snapshot)
	}
	got, err := os.ReadFile(s.snapshotFilePath("snap-1"))
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("expected the snapshot to contain the source volume's real content, got %q", got)
	}
}

// TestCreateSnapshotIsIdempotentOnRetry mirrors
// TestCreateVolumeIsIdempotentOnRetry's own reasoning: a retry with the
// same Name must succeed without re-copying.
func TestCreateSnapshotIsIdempotentOnRetry(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("cp", fakeCPHandler)
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-src", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := os.WriteFile(s.backingFilePath("pvc-src"), []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}
	req := &csi.CreateSnapshotRequest{Name: "snap-retry", SourceVolumeId: created.Volume.VolumeId}
	if _, err := s.CreateSnapshot(context.Background(), req); err != nil {
		t.Fatalf("first CreateSnapshot: %v", err)
	}
	before := len(run.callsFor("cp"))
	if _, err := s.CreateSnapshot(context.Background(), req); err != nil {
		t.Fatalf("retried CreateSnapshot: %v", err)
	}
	if after := len(run.callsFor("cp")); after != before {
		t.Fatalf("expected no new copy on a retry with the same name, got %d new", after-before)
	}
}

func TestDeleteSnapshotRequiresSnapshotID(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{}); err == nil {
		t.Fatal("expected an error for an empty snapshot_id")
	}
}

func TestDeleteSnapshotRemovesTheFile(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("cp", fakeCPHandler)
	s := newTestControllerServer(t, run)
	created, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-src", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if err := os.WriteFile(s.backingFilePath("pvc-src"), []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}
	if _, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-del", SourceVolumeId: created.Volume.VolumeId}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	if _, err := s.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "snap-del"}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, err := os.Stat(s.snapshotFilePath("snap-del")); !os.IsNotExist(err) {
		t.Fatalf("expected the snapshot file to be gone, stat err=%v", err)
	}
}

// TestDeleteSnapshotIsIdempotentOnUnknownID mirrors DeleteVolume's own
// "never error on an already-gone/unrecognized ID" CSI requirement.
func TestDeleteSnapshotIsIdempotentOnUnknownID(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := s.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "never-created"}); err != nil {
		t.Fatalf("expected DeleteSnapshot to succeed on an unknown snapshot_id, got %v", err)
	}
}

// TestCreateVolumeRestoresFromSnapshot exercises the other half of the
// snapshot feature: a new volume created with VolumeContentSource naming
// an existing snapshot gets that snapshot's real content copied into its
// own backing file before the LIO backstore is created around it.
func TestCreateVolumeRestoresFromSnapshot(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("cp", fakeCPHandler)
	s := newTestControllerServer(t, run)
	srcVol, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-src", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("CreateVolume (source): %v", err)
	}
	content := []byte("snapshot restore content")
	if err := os.WriteFile(s.backingFilePath("pvc-src"), content, 0o600); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}
	if _, err := s.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-restore", SourceVolumeId: srcVol.Volume.VolumeId}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	restored, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-restored",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		// Deliberately smaller than the snapshot's own real size, to
		// prove CreateVolume floors it up rather than honoring a
		// requested size smaller than the content being restored.
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-restore"},
		}},
	})
	if err != nil {
		t.Fatalf("CreateVolume (restore): %v", err)
	}
	if restored.Volume.CapacityBytes != int64(len(content)) {
		t.Fatalf("expected the restored volume's size to floor at the snapshot's own size, got %d", restored.Volume.CapacityBytes)
	}
	got, err := os.ReadFile(s.backingFilePath("pvc-restored"))
	if err != nil {
		t.Fatalf("read restored backing file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("expected the restored volume's backing file to contain the snapshot's real content, got %q", got)
	}
}

func TestCreateVolumeRejectsUnknownSnapshotSource(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	_, err := s.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:               "pvc-restored",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "never-created"},
		}},
	})
	if err == nil {
		t.Fatal("expected an error when the named snapshot doesn't exist")
	}
}
