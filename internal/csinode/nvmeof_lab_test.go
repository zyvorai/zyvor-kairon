// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build lab

// Real-kernel NVMe-oF checks, run as root by scripts/lab-nvmeof.sh: the
// Controller and Node servers in-process against the host's nvmet configfs,
// /dev/nvme-fabrics and native multipath. Not part of go test ./...

package csinode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
)

type labEnv struct {
	ctrl *ControllerServer
	node *NodeServer
	dir  string
}

func newLabEnv(t *testing.T) *labEnv {
	t.Helper()
	portals := os.Getenv("KAIRON_LAB_NVME_PORTALS")
	dir := os.Getenv("KAIRON_LAB_DIR")
	if portals == "" || dir == "" {
		t.Skip("KAIRON_LAB_NVME_PORTALS and KAIRON_LAB_DIR are not set")
	}
	ps, err := ParseNVMePortals(portals)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := NewControllerServer(NewCommandRunner(), "127.0.0.1:3260", filepath.Join(dir, "volumes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ctrl.EnableNVMe(ps, "/sys/kernel/config/nvmet"); err != nil {
		t.Fatal(err)
	}
	return &labEnv{ctrl: ctrl, node: NewNodeServer("lab", NewCommandRunner()), dir: dir}
}

func labMountCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

// create provisions, stages and publishes name; the returned func tears it
// all down and checks nothing is left behind.
func (e *labEnv) create(t *testing.T, name, transport string, size int64, capab *csi.VolumeCapability) (nvmeConfig, string, string, func()) {
	t.Helper()
	ctx := context.Background()
	resp, err := e.ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               name,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: size},
		VolumeCapabilities: []*csi.VolumeCapability{capab},
		Parameters:         map[string]string{"transport": transport},
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	id := resp.GetVolume().GetVolumeId()
	ref, err := decodeAnyVolumeID(id)
	if err != nil || ref.nvme == nil {
		t.Fatalf("volume id %q: %v", id, err)
	}
	// Backstop for a failure before the returned teardown runs.
	t.Cleanup(func() {
		_ = e.node.nvme.disconnect(ref.nvme.NQN)
		_, _ = e.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id})
	})
	staging := filepath.Join(e.dir, name, "staging")
	target := filepath.Join(e.dir, name, "target")
	if _, err := e.node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: id, StagingTargetPath: staging, VolumeCapability: capab, VolumeContext: resp.GetVolume().GetVolumeContext()}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	if _, err := e.node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: id, StagingTargetPath: staging, TargetPath: target, VolumeCapability: capab}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	t.Logf("%s: volume %s staged at %s, published at %s", name, id, staging, target)
	return *ref.nvme, staging, target, func() {
		if _, err := e.node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: id, TargetPath: target}); err != nil {
			t.Errorf("NodeUnpublishVolume: %v", err)
		}
		if _, err := e.node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: id, StagingTargetPath: staging}); err != nil {
			t.Errorf("NodeUnstageVolume: %v", err)
		}
		if paths := e.node.nvme.paths(ref.nvme.NQN); len(paths) != 0 {
			t.Errorf("controllers left after unstage: %+v", paths)
		}
		if _, err := e.ctrl.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Errorf("DeleteVolume: %v", err)
		}
		if _, err := os.Stat(e.ctrl.nvmet.subsystemDir(ref.nvme.NQN)); !os.IsNotExist(err) {
			t.Errorf("subsystem %s left in configfs", ref.nvme.NQN)
		}
	}
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func writeSync(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("fsync %s: %v", path, err)
	}
	_ = f.Close()
}

func portIDFor(t *testing.T, e *labEnv, addr string) int {
	for i, p := range e.ctrl.NVMePortals {
		if p.addr() == addr {
			return nvmetPortIDBase + i
		}
	}
	t.Fatalf("no portal %s", addr)
	return 0
}

// TestLabNVMeTCPMultipath: a filesystem volume over every tcp portal, ANA
// and link-loss failover with I/O continuing, then online expansion.
func TestLabNVMeTCPMultipath(t *testing.T) {
	e := newLabEnv(t)
	cfg, _, target, cleanup := e.create(t, "pvc-lab-tcp", "nvme-tcp", 256<<20, labMountCap())
	defer cleanup()
	if len(cfg.Portals) < 2 {
		t.Fatalf("need two tcp portals for multipath, got %v", cfg.Portals)
	}
	paths := e.node.nvme.paths(cfg.NQN)
	abnormal, msg := e.node.nvme.pathCondition(cfg)
	t.Logf("connected: %s", msg)
	if len(paths) != len(cfg.Portals) || abnormal {
		t.Fatalf("expected %d usable paths: %s", len(cfg.Portals), msg)
	}
	dev := e.node.nvme.namespaceDevice(cfg.NQN)
	if !strings.HasPrefix(filepath.Base(dev), "nvme") || strings.Contains(filepath.Base(dev), "c") {
		t.Fatalf("expected the multipath head device, got %q", dev)
	}
	writeSync(t, filepath.Join(target, "before"), []byte("one"))

	// ANA: the first portal's group goes inaccessible; I/O moves to the rest.
	first := portIDFor(t, e, cfg.Portals[0])
	ana := filepath.Join(e.ctrl.nvmet.portDir(first), "ana_groups", "1", "ana_state")
	if err := writeAttr(ana, "inaccessible"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ANA change to reach the host", 30*time.Second, func() bool {
		ab, _ := e.node.nvme.pathCondition(cfg)
		return ab
	})
	_, msg = e.node.nvme.pathCondition(cfg)
	t.Logf("ANA inaccessible on %s: %s", cfg.Portals[0], msg)
	writeSync(t, filepath.Join(target, "during-ana"), bytes.Repeat([]byte("a"), 1<<20))
	if err := writeAttr(ana, "optimized"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ANA optimized again", 30*time.Second, func() bool {
		ab, _ := e.node.nvme.pathCondition(cfg)
		return !ab
	})

	// Link loss: unexport from the first portal; that controller reconnects
	// in the background (ctrl_loss_tmo=-1) while I/O uses the others.
	if err := e.ctrl.nvmet.unlinkPort(first, cfg.NQN); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the lost path to show", 30*time.Second, func() bool {
		ab, _ := e.node.nvme.pathCondition(cfg)
		return ab
	})
	_, msg = e.node.nvme.pathCondition(cfg)
	t.Logf("path lost: %s", msg)
	start := time.Now()
	writeSync(t, filepath.Join(target, "during-loss"), bytes.Repeat([]byte("b"), 1<<20))
	t.Logf("1 MiB write+fsync with one path down took %s", time.Since(start).Round(time.Millisecond))
	if err := e.ctrl.nvmet.linkPort(first, cfg.NQN); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the path to reconnect", 90*time.Second, func() bool {
		ab, _ := e.node.nvme.pathCondition(cfg)
		return !ab
	})
	_, msg = e.node.nvme.pathCondition(cfg)
	t.Logf("path back: %s", msg)

	// Online expansion.
	ctx := context.Background()
	id := encodeNVMeVolumeID(cfg)
	if _, err := e.ctrl.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: id, CapacityRange: &csi.CapacityRange{RequiredBytes: 512 << 20}}); err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if _, err := e.node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumeId: id, VolumePath: target, CapacityRange: &csi.CapacityRange{RequiredBytes: 512 << 20}}); err != nil {
		t.Fatalf("NodeExpandVolume: %v", err)
	}
	stats, err := e.node.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: id, VolumePath: target})
	if err != nil {
		t.Fatal(err)
	}
	total := stats.GetUsage()[0].GetTotal()
	t.Logf("filesystem after expand: %d MiB", total>>20)
	if total < 400<<20 {
		t.Fatalf("filesystem did not grow: %d bytes", total)
	}
	for _, f := range []string{"before", "during-ana", "during-loss"} {
		if _, err := os.Stat(filepath.Join(target, f)); err != nil {
			t.Error(err)
		}
	}
}

// TestLabNVMeRDMABlock: a raw Block volume over nvme-rdma (soft-RoCE in the
// lab), written and read back through the published device file.
func TestLabNVMeRDMABlock(t *testing.T) {
	e := newLabEnv(t)
	if !strings.Contains(os.Getenv("KAIRON_LAB_NVME_PORTALS"), "rdma:") {
		t.Skip("no rdma portal")
	}
	cfg, staging, target, cleanup := e.create(t, "pvc-lab-rdma", "nvme-rdma", 128<<20, blockCap())
	defer cleanup()
	_, msg := e.node.nvme.pathCondition(cfg)
	t.Logf("connected: %s", msg)
	for _, p := range e.node.nvme.paths(cfg.NQN) {
		if p.Transport != "rdma" {
			t.Fatalf("path over %s, not rdma: %+v", p.Transport, p)
		}
	}
	if !isDevice(StagedDevicePath(staging)) || !isDevice(target) {
		t.Fatalf("staged link or published file is not a block device")
	}
	pattern := bytes.Repeat([]byte("kairon-rdma!"), 4096/12+1)[:4096]
	f, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(pattern, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// Read back with O_DIRECT so the data comes off the device, not the
	// page cache the write went through.
	buf := make([]byte, 2*len(pattern))
	off := int(uintptr(unsafe.Pointer(&buf[0])) & 4095)
	if off != 0 {
		off = 4096 - off
	}
	got := buf[off : off+len(pattern)]
	fd, err := unix.Open(StagedDevicePath(staging), unix.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if n, err := unix.Pread(fd, got, 64<<20); err != nil || n != len(got) || !bytes.Equal(got, pattern) {
		t.Fatalf("O_DIRECT read back mismatch: n=%d %v", n, err)
	}
	size, err := blockSize(target)
	if err != nil || size != 128<<20 {
		t.Fatalf("block size %d: %v", size, err)
	}
}

// TestLabNVMeProvision creates volumes for the Machine steps of the lab
// without staging them: KAIRON_LAB_PROVISION is "name:MiB,..." and
// KAIRON_LAB_PROVISION_OUT gets one "name volume-id bytes" line each.
func TestLabNVMeProvision(t *testing.T) {
	spec, out := os.Getenv("KAIRON_LAB_PROVISION"), os.Getenv("KAIRON_LAB_PROVISION_OUT")
	if spec == "" || out == "" {
		t.Skip("KAIRON_LAB_PROVISION / KAIRON_LAB_PROVISION_OUT are not set")
	}
	e := newLabEnv(t)
	items := strings.Split(spec, ",")
	lines := make([]string, 0, len(items))
	for _, item := range items {
		name, mib, _ := strings.Cut(item, ":")
		var n int64
		if _, err := fmt.Sscan(mib, &n); err != nil || n <= 0 {
			t.Fatalf("bad size in %q", item)
		}
		resp, err := e.ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name: name, CapacityRange: &csi.CapacityRange{RequiredBytes: n << 20},
			VolumeCapabilities: []*csi.VolumeCapability{blockCap()}, Parameters: map[string]string{"transport": "nvme-tcp"},
		})
		if err != nil {
			t.Fatalf("CreateVolume %s: %v", name, err)
		}
		lines = append(lines, fmt.Sprintf("%s %s %d", name, resp.GetVolume().GetVolumeId(), resp.GetVolume().GetCapacityBytes()))
	}
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLabNVMeRelease disconnects and deletes the volume ids in
// KAIRON_LAB_RELEASE (comma-separated).
func TestLabNVMeRelease(t *testing.T) {
	ids := os.Getenv("KAIRON_LAB_RELEASE")
	if ids == "" {
		t.Skip("KAIRON_LAB_RELEASE is not set")
	}
	e := newLabEnv(t)
	for _, id := range strings.Split(ids, ",") {
		if ref, err := decodeAnyVolumeID(id); err == nil && ref.nvme != nil {
			if err := e.node.nvme.disconnect(ref.nvme.NQN); err != nil {
				t.Error(err)
			}
		}
		if _, err := e.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Errorf("DeleteVolume %s: %v", id, err)
		}
	}
}
