// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestNodeServer builds a NodeServer with every OS-touching seam
// stubbed out -- see NodeServer's own doc comment for why. mounted tracks
// which paths the fake considers mounted, so a test can assert
// NodeStageVolume/NodePublishVolume actually called mountFn (by checking
// the path was added) without ever touching a real filesystem or mount
// syscall.
type testNodeServerFixture struct {
	server  *NodeServer
	run     *fakeCommandRunner
	mounted map[string]bool
}

func newTestNodeServer(t *testing.T) *testNodeServerFixture {
	t.Helper()
	run := newFakeCommandRunner()
	f := &testNodeServerFixture{run: run, mounted: map[string]bool{}}
	f.server = &NodeServer{
		NodeID: "test-node",
		Runner: run,
		iscsi:  &iscsiClient{run: run},
		isMountPoint: func(path string) (bool, error) {
			return f.mounted[path], nil
		},
		mountFn: func(source, target, fsType string, flags uintptr, data string) error {
			f.mounted[target] = true
			return nil
		},
		unmountFn: func(target string) error {
			delete(f.mounted, target)
			return nil
		},
		statfsFn: func(path string) (VolumeStats, error) {
			return VolumeStats{}, errors.New("statfsFn not stubbed for this test")
		},
		blockSizeFn: func(path string) (int64, error) { return 0, errors.New("blockSizeFn not stubbed") },
		isDeviceFn:  func(path string) bool { return false },
		nvme:        newNVMeHost(),
	}
	return f
}

func mountVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func TestNodeGetInfoReportsConfiguredNodeID(t *testing.T) {
	f := newTestNodeServer(t)
	f.server.NodeID = "worker-7"
	resp, err := f.server.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.GetNodeId() != "worker-7" {
		t.Fatalf("expected node_id worker-7, got %q", resp.GetNodeId())
	}
}

func TestNodeGetCapabilitiesReportsStageUnstage(t *testing.T) {
	f := newTestNodeServer(t)
	resp, err := f.server.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}
	want := map[csi.NodeServiceCapability_RPC_Type]bool{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME: false,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME:        false,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS:     false,
	}
	if len(resp.GetCapabilities()) != len(want) {
		t.Fatalf("expected exactly %d capabilities, got %+v", len(want), resp.GetCapabilities())
	}
	for _, c := range resp.GetCapabilities() {
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

func setupLoginFixture(t *testing.T, f *testNodeServerFixture) {
	t.Helper()
	dir := t.TempDir()
	realDevice := dir + "/sda"
	if err := writeFile(realDevice, "x"); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	symlink := dir + "/by-path-link"
	if err := symlinkFile(realDevice, symlink); err != nil {
		t.Fatalf("symlink fake device: %v", err)
	}
	t.Cleanup(setDevicePathForTest(func(iscsiConfig) string { return symlink }))
	f.run.on("blkid", func(args ...string) (string, error) { return "ext4", nil }) // already formatted
}

func TestNodeStageVolumeRequiresFields(t *testing.T) {
	f := newTestNodeServer(t)
	cases := []*csi.NodeStageVolumeRequest{
		{StagingTargetPath: "/x", VolumeCapability: mountVolumeCapability()},
		{VolumeId: "v1", VolumeCapability: mountVolumeCapability()},
		{VolumeId: "v1", StagingTargetPath: "/x"}, // no VolumeCapability
	}
	for i, req := range cases {
		if _, err := f.server.NodeStageVolume(context.Background(), req); err == nil {
			t.Fatalf("case %d: expected an error for an incomplete request", i)
		}
	}
}

func TestNodeStageVolumeRejectsBlockAccessType(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "v1",
		StagingTargetPath: t.TempDir(),
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		VolumeContext: map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"},
	}
	if _, err := f.server.NodeStageVolume(context.Background(), req); err == nil {
		t.Fatal("expected raw block volumes to be rejected in this first cut")
	}
}

func TestNodeStageVolumeLogsInFormatsAndMounts(t *testing.T) {
	f := newTestNodeServer(t)
	setupLoginFixture(t, f)
	staging := t.TempDir() + "/stage"

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          encodeVolumeID(iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}),
		StagingTargetPath: staging,
		VolumeCapability:  mountVolumeCapability(),
		VolumeContext:     map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"},
	}
	if _, err := f.server.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if !f.mounted[staging] {
		t.Fatal("expected the staging path to be mounted")
	}
	if len(f.run.callsFor("iscsiadm")) == 0 {
		t.Fatal("expected at least one iscsiadm call")
	}
}

func TestNodeStageVolumeIsIdempotent(t *testing.T) {
	f := newTestNodeServer(t)
	staging := t.TempDir() + "/stage"
	f.mounted[staging] = true // already staged, as if a previous call succeeded

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "v1",
		StagingTargetPath: staging,
		VolumeCapability:  mountVolumeCapability(),
		VolumeContext:     map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"},
	}
	if _, err := f.server.NodeStageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if len(f.run.calls) != 0 {
		t.Fatalf("expected no commands to run against an already-staged volume, got %v", f.run.calls)
	}
}

func TestNodeStageVolumePropagatesLoginFailure(t *testing.T) {
	f := newTestNodeServer(t)
	f.run.on("iscsiadm", func(args ...string) (string, error) { return "", errors.New("boom") })
	t.Cleanup(setDeviceWaitTimeoutForTest())

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "v1",
		StagingTargetPath: t.TempDir() + "/stage",
		VolumeCapability:  mountVolumeCapability(),
		VolumeContext:     map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"},
	}
	if _, err := f.server.NodeStageVolume(context.Background(), req); err == nil {
		t.Fatal("expected an iscsi login failure to be propagated")
	}
}

func TestNodeUnstageVolumeUnmountsAndLogsOut(t *testing.T) {
	f := newTestNodeServer(t)
	staging := t.TempDir() + "/stage"
	f.mounted[staging] = true

	req := &csi.NodeUnstageVolumeRequest{
		VolumeId:          encodeVolumeID(iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}),
		StagingTargetPath: staging,
	}
	if _, err := f.server.NodeUnstageVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}
	if f.mounted[staging] {
		t.Fatal("expected the staging path to be unmounted")
	}
	if len(f.run.callsFor("iscsiadm")) == 0 {
		t.Fatal("expected a logout iscsiadm call")
	}
}

func TestNodeUnstageVolumeIsIdempotentWhenNotMounted(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodeUnstageVolumeRequest{
		VolumeId:          encodeVolumeID(iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}),
		StagingTargetPath: t.TempDir() + "/never-mounted",
	}
	if _, err := f.server.NodeUnstageVolume(context.Background(), req); err != nil {
		t.Fatalf("expected NodeUnstageVolume to succeed against an already-unstaged path, got: %v", err)
	}
}

func TestNodeUnstageVolumeRejectsUnrecognizedVolumeID(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodeUnstageVolumeRequest{VolumeId: "not-a-kairon-handle", StagingTargetPath: t.TempDir()}
	if _, err := f.server.NodeUnstageVolume(context.Background(), req); err == nil {
		t.Fatal("expected an error for a volume_id this driver didn't mint")
	}
}

func TestNodePublishVolumeBindMounts(t *testing.T) {
	f := newTestNodeServer(t)
	staging := t.TempDir() + "/stage"
	target := t.TempDir() + "/target"

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          "v1",
		StagingTargetPath: staging,
		TargetPath:        target,
		VolumeCapability:  mountVolumeCapability(),
	}
	if _, err := f.server.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	if !f.mounted[target] {
		t.Fatal("expected the target path to be bind-mounted")
	}
}

func TestNodePublishVolumeIsIdempotent(t *testing.T) {
	f := newTestNodeServer(t)
	target := t.TempDir() + "/target"
	f.mounted[target] = true

	req := &csi.NodePublishVolumeRequest{
		VolumeId: "v1", StagingTargetPath: t.TempDir() + "/stage", TargetPath: target, VolumeCapability: mountVolumeCapability(),
	}
	if _, err := f.server.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
}

func TestNodePublishVolumeRequiresStagingPath(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodePublishVolumeRequest{VolumeId: "v1", TargetPath: t.TempDir(), VolumeCapability: mountVolumeCapability()}
	if _, err := f.server.NodePublishVolume(context.Background(), req); err == nil {
		t.Fatal("expected an error when staging_target_path is missing")
	}
}

func TestNodeUnpublishVolumeUnmounts(t *testing.T) {
	f := newTestNodeServer(t)
	target := t.TempDir() + "/target"
	f.mounted[target] = true

	req := &csi.NodeUnpublishVolumeRequest{VolumeId: "v1", TargetPath: target}
	if _, err := f.server.NodeUnpublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if f.mounted[target] {
		t.Fatal("expected the target path to be unmounted")
	}
}

func TestNodeUnpublishVolumeIsIdempotentWhenNotMounted(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodeUnpublishVolumeRequest{VolumeId: "v1", TargetPath: t.TempDir() + "/never-mounted"}
	if _, err := f.server.NodeUnpublishVolume(context.Background(), req); err != nil {
		t.Fatalf("expected NodeUnpublishVolume to succeed against an already-unpublished path, got: %v", err)
	}
}

func TestNodeExpandVolumeRequiresFields(t *testing.T) {
	f := newTestNodeServer(t)
	if _, err := f.server.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumePath: "/x"}); err == nil {
		t.Fatal("expected an error for an empty volume_id")
	}
	if _, err := f.server.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: "v1"}); err == nil {
		t.Fatal("expected an error for an empty volume_path")
	}
}

// setupExpandFixture points devicePathFunc at a real symlink to a fake
// device file, the same seam setupLoginFixture already establishes for
// NodeStageVolume's own tests.
func setupExpandFixture(t *testing.T) (volumeID string) {
	t.Helper()
	dir := t.TempDir()
	realDevice := dir + "/sda"
	if err := writeFile(realDevice, "x"); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	symlink := dir + "/by-path-link"
	if err := symlinkFile(realDevice, symlink); err != nil {
		t.Fatalf("symlink fake device: %v", err)
	}
	t.Cleanup(setDevicePathForTest(func(iscsiConfig) string { return symlink }))
	return "iscsi|10.0.0.5:3260|iqn.2026-01.dev.zyvor.kairon:vol1|0"
}

func TestNodeExpandVolumeRescansAndGrowsExt4(t *testing.T) {
	f := newTestNodeServer(t)
	volumeID := setupExpandFixture(t)
	f.run.on("blkid", func(args ...string) (string, error) { return "ext4", nil })

	if _, err := f.server.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volumeID, VolumePath: "/staged",
	}); err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	if calls := f.run.callsFor("iscsiadm"); len(calls) != 1 || joinArgs(calls[0][1:]) != "-m node -T iqn.2026-01.dev.zyvor.kairon:vol1 -p 10.0.0.5:3260 -R" {
		t.Fatalf("expected exactly one rescan call, got %+v", calls)
	}
	if calls := f.run.callsFor("resize2fs"); len(calls) != 1 {
		t.Fatalf("expected exactly one resize2fs call, got %+v", calls)
	}
	if calls := f.run.callsFor("xfs_growfs"); len(calls) != 0 {
		t.Fatalf("expected no xfs_growfs call for an ext4 filesystem, got %+v", calls)
	}
}

func TestNodeExpandVolumeGrowsXFSViaVolumePath(t *testing.T) {
	f := newTestNodeServer(t)
	volumeID := setupExpandFixture(t)
	f.run.on("blkid", func(args ...string) (string, error) { return "xfs", nil })

	if _, err := f.server.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volumeID, VolumePath: "/staged",
	}); err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	calls := f.run.callsFor("xfs_growfs")
	if len(calls) != 1 || joinArgs(calls[0][1:]) != "/staged" {
		t.Fatalf("expected xfs_growfs to run against the volume path, got %+v", calls)
	}
}

func TestNodeExpandVolumeRejectsUnsupportedFilesystem(t *testing.T) {
	f := newTestNodeServer(t)
	volumeID := setupExpandFixture(t)
	f.run.on("blkid", func(args ...string) (string, error) { return "btrfs", nil })

	if _, err := f.server.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: volumeID, VolumePath: "/staged",
	}); err == nil {
		t.Fatal("expected an error for a filesystem this driver doesn't know how to grow")
	}
}

func TestNodeGetVolumeStatsRequiresFields(t *testing.T) {
	f := newTestNodeServer(t)
	if _, err := f.server.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumePath: "/x"}); err == nil {
		t.Fatal("expected an error for an empty volume_id")
	}
	if _, err := f.server.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v1"}); err == nil {
		t.Fatal("expected an error for an empty volume_path")
	}
}

func TestNodeGetVolumeStatsReturnsNotFoundWhenNotMounted(t *testing.T) {
	f := newTestNodeServer(t)
	req := &csi.NodeGetVolumeStatsRequest{VolumeId: "v1", VolumePath: t.TempDir() + "/never-mounted"}
	_, err := f.server.NodeGetVolumeStats(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error for a volume path that isn't currently mounted")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestNodeGetVolumeStatsReportsUsage(t *testing.T) {
	f := newTestNodeServer(t)
	path := t.TempDir() + "/staged"
	f.mounted[path] = true
	f.server.statfsFn = func(got string) (VolumeStats, error) {
		if got != path {
			t.Fatalf("expected statfs to be called with %q, got %q", path, got)
		}
		return VolumeStats{
			TotalBytes: 1000, UsedBytes: 400, AvailableBytes: 600,
			TotalInodes: 100, UsedInodes: 10, AvailableInodes: 90,
		}, nil
	}

	resp, err := f.server.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v1", VolumePath: path})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	usage := resp.GetUsage()
	if len(usage) != 2 {
		t.Fatalf("expected exactly 2 usage entries (bytes, inodes), got %+v", usage)
	}
	byUnit := map[csi.VolumeUsage_Unit]*csi.VolumeUsage{}
	for _, u := range usage {
		byUnit[u.GetUnit()] = u
	}
	bytes, ok := byUnit[csi.VolumeUsage_BYTES]
	if !ok {
		t.Fatal("expected a BYTES usage entry")
	}
	if bytes.GetTotal() != 1000 || bytes.GetUsed() != 400 || bytes.GetAvailable() != 600 {
		t.Fatalf("unexpected bytes usage: %+v", bytes)
	}
	inodes, ok := byUnit[csi.VolumeUsage_INODES]
	if !ok {
		t.Fatal("expected an INODES usage entry")
	}
	if inodes.GetTotal() != 100 || inodes.GetUsed() != 10 || inodes.GetAvailable() != 90 {
		t.Fatalf("unexpected inodes usage: %+v", inodes)
	}
}

func TestNodeGetVolumeStatsPropagatesStatfsError(t *testing.T) {
	f := newTestNodeServer(t)
	path := t.TempDir() + "/staged"
	f.mounted[path] = true
	f.server.statfsFn = func(string) (VolumeStats, error) {
		return VolumeStats{}, errors.New("statfs: input/output error")
	}

	_, err := f.server.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: "v1", VolumePath: path})
	if err == nil {
		t.Fatal("expected the statfs error to propagate rather than fabricating stats")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}
