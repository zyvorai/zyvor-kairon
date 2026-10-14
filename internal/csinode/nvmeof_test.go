// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestParseNVMePortals(t *testing.T) {
	got, err := ParseNVMePortals("tcp:10.0.0.1:4420, tcp:10.0.0.2:4420,rdma:[fd00::1]:4421")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Transport != transportNVMeTCP || got[2].Transport != transportNVMeRDMA || got[2].addr() != "[fd00::1]:4421" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"tcp", "fc:1.2.3.4:1", "tcp:host.example:4420", "tcp:1.2.3.4:x", "tcp:1.2.3.4"} {
		if _, err := ParseNVMePortals(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestNVMeVolumeIDRoundTrip(t *testing.T) {
	cfg := nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "pvc-1", Portals: []string{"10.0.0.1:4420", "10.0.0.2:4420"}}
	id := encodeNVMeVolumeID(cfg)
	if id != "nvme|nvme-tcp|nqn.2026-01.dev.zyvor.kairon:pvc-1|10.0.0.1:4420,10.0.0.2:4420" {
		t.Fatal(id)
	}
	ref, err := decodeAnyVolumeID(id)
	if err != nil || ref.nvme == nil || len(ref.nvme.Portals) != 2 || ref.nvme.NQN != cfg.NQN {
		t.Fatalf("%+v %v", ref, err)
	}
	ref, err = decodeAnyVolumeID("iscsi|1.2.3.4:3260|iqn.x|0")
	if err != nil || ref.iscsi == nil {
		t.Fatalf("%+v %v", ref, err)
	}
	if _, err := decodeAnyVolumeID("nvme|nvme-fc|nqn.x|1.2.3.4:1"); err == nil {
		t.Fatal("unknown transport accepted")
	}
	name, ok, err := volumeNameFromID(id)
	if err != nil || !ok || name != "pvc-1" {
		t.Fatal(name, ok, err)
	}
}

func newTestNVMet(t *testing.T) *nvmetClient {
	root := t.TempDir()
	for _, d := range []string{"ports", "subsystems"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &nvmetClient{Root: root, rmdir: os.RemoveAll}
}

func TestNVMetSubsystemLifecycle(t *testing.T) {
	c := newTestNVMet(t)
	nqn := nqnPrefix + "pvc-a"
	p := NVMePortal{Transport: transportNVMeTCP, Host: "10.0.0.1", Port: "4420"}
	if err := c.ensureSubsystem(nqn, "/vols/pvc-a.img"); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureSubsystem(nqn, "/vols/pvc-a.img"); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := c.ensureSubsystem(nqn, "/vols/other.img"); err == nil {
		t.Fatal("a different backing file for an enabled namespace was accepted")
	}
	ns := filepath.Join(c.subsystemDir(nqn), "namespaces", "1")
	for attr, want := range map[string]string{"device_path": "/vols/pvc-a.img", "enable": "1", "device_uuid": namespaceUUID(nqn)} {
		if got := readAttr(filepath.Join(ns, attr)); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}
	if readAttr(filepath.Join(c.subsystemDir(nqn), "attr_allow_any_host")) != "1" {
		t.Error("allow_any_host not set")
	}
	if err := c.ensurePort(nvmetPortIDBase, p); err != nil {
		t.Fatal(err)
	}
	if err := c.linkPort(nvmetPortIDBase, nqn); err != nil {
		t.Fatal(err)
	}
	if err := c.linkPort(nvmetPortIDBase, nqn); err != nil {
		t.Fatalf("link not idempotent: %v", err)
	}
	if got := readAttr(filepath.Join(c.portDir(nvmetPortIDBase), "addr_trtype")); got != "tcp" {
		t.Errorf("trtype %q", got)
	}
	if err := c.ensurePort(nvmetPortIDBase, p); err != nil {
		t.Fatalf("same address re-ensured: %v", err)
	}
	moved := p
	moved.Host = "10.0.0.9"
	if err := c.ensurePort(nvmetPortIDBase, moved); err == nil {
		t.Fatal("changing a port with linked subsystems was accepted")
	}
	if err := c.revalidateSize(nqn); err != nil {
		t.Fatal(err)
	}
	if err := c.deleteSubsystem(nqn); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(c.portDir(nvmetPortIDBase), "subsystems", nqn)); !os.IsNotExist(err) {
		t.Error("port link left behind")
	}
	if _, err := os.Stat(c.subsystemDir(nqn)); !os.IsNotExist(err) {
		t.Error("subsystem left behind")
	}
	if err := c.deleteSubsystem(nqn); err != nil {
		t.Fatalf("delete not idempotent: %v", err)
	}
}

func TestNamespaceUUIDIsStableAndVersioned(t *testing.T) {
	a, b := namespaceUUID(nqnPrefix+"x"), namespaceUUID(nqnPrefix+"x")
	if a != b || len(a) != 36 || a[14] != '5' {
		t.Fatal(a, b)
	}
	if namespaceUUID(nqnPrefix+"y") == a {
		t.Fatal("collision")
	}
}

// fakeNVMeSys is a /sys/class/nvme + nvme-subsystem tree whose connect hook
// creates a controller the way the kernel would.
type fakeNVMeSys struct {
	t        *testing.T
	host     *nvmeHost
	connects []string
	refuse   map[string]bool // traddr:trsvcid that refuse connects
	next     int
}

func newFakeNVMeSys(t *testing.T) *fakeNVMeSys {
	root := t.TempDir()
	h := &nvmeHost{
		SysNVMe:       filepath.Join(root, "class", "nvme"),
		SysSubsystem:  filepath.Join(root, "class", "nvme-subsystem"),
		MultipathFile: filepath.Join(root, "multipath"),
		HostNQNFile:   filepath.Join(root, "hostnqn"),
		HostIDFile:    filepath.Join(root, "hostid"),
		MachineIDFile: filepath.Join(root, "machine-id"),
		DevDir:        "/dev",
	}
	for _, d := range []string{h.SysNVMe, h.SysSubsystem} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(h.MachineIDFile, []byte("0123456789abcdef0123456789abcdef\n"), 0o644)
	_ = os.WriteFile(h.MultipathFile, []byte("Y\n"), 0o644)
	f := &fakeNVMeSys{t: t, host: h, refuse: map[string]bool{}}
	h.connectFn = f.connect
	return f
}

func (f *fakeNVMeSys) connect(opts string) error {
	f.connects = append(f.connects, opts)
	kv := map[string]string{}
	for _, p := range strings.Split(opts, ",") {
		k, v, _ := strings.Cut(p, "=")
		kv[k] = v
	}
	if f.refuse[kv["traddr"]+":"+kv["trsvcid"]] {
		return fmt.Errorf("connection refused")
	}
	ctrl := fmt.Sprintf("nvme%d", f.next)
	dir := filepath.Join(f.host.SysNVMe, ctrl)
	ns := filepath.Join(dir, fmt.Sprintf("nvme0c%dn1", f.next))
	f.next++
	must := func(err error) {
		if err != nil {
			f.t.Fatal(err)
		}
	}
	must(os.MkdirAll(ns, 0o755))
	must(os.WriteFile(filepath.Join(dir, "subsysnqn"), []byte(kv["nqn"]+"\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "transport"), []byte(kv["transport"]+"\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "address"), []byte(fmt.Sprintf("traddr=%s,trsvcid=%s,src_addr=10.9.9.9\n", kv["traddr"], kv["trsvcid"])), 0o644))
	must(os.WriteFile(filepath.Join(dir, "state"), []byte("live\n"), 0o644))
	must(os.WriteFile(filepath.Join(ns, "ana_state"), []byte("optimized\n"), 0o644))
	sub := filepath.Join(f.host.SysSubsystem, "nvme-subsys0")
	must(os.MkdirAll(filepath.Join(sub, "nvme0n1"), 0o755))
	must(os.WriteFile(filepath.Join(sub, "subsysnqn"), []byte(kv["nqn"]+"\n"), 0o644))
	return nil
}

func TestNVMeHostConnectsEveryPathOnce(t *testing.T) {
	f := newFakeNVMeSys(t)
	cfg := nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "v", Portals: []string{"10.0.0.1:4420", "10.0.0.2:4420"}}
	dev, err := f.host.connect(context.Background(), cfg)
	if err != nil || dev != "/dev/nvme0n1" {
		t.Fatal(dev, err)
	}
	if len(f.connects) != 2 || !strings.Contains(f.connects[0], "transport=tcp,traddr=10.0.0.1,trsvcid=4420,hostnqn=nqn.2014-08.org.nvmexpress:uuid:01234567-89ab-cdef-0123-456789abcdef,hostid=01234567-89ab-cdef-0123-456789abcdef,ctrl_loss_tmo=-1") {
		t.Fatalf("%q", f.connects)
	}
	if _, err := f.host.connect(context.Background(), cfg); err != nil || len(f.connects) != 2 {
		t.Fatalf("reconnected existing paths: %q %v", f.connects, err)
	}
	abnormal, msg := f.host.pathCondition(cfg)
	if abnormal || !strings.Contains(msg, "2 of 2 paths usable") || !strings.Contains(msg, "nvme1 tcp 10.0.0.2:4420 live optimized") {
		t.Fatal(abnormal, msg)
	}
	_ = os.WriteFile(filepath.Join(f.host.SysNVMe, "nvme1", "state"), []byte("connecting\n"), 0o644)
	if abnormal, msg = f.host.pathCondition(cfg); !abnormal || !strings.Contains(msg, "1 of 2 paths usable") {
		t.Fatal(abnormal, msg)
	}
	_ = os.WriteFile(filepath.Join(f.host.SysNVMe, "nvme1", "state"), []byte("live\n"), 0o644)
	_ = os.WriteFile(filepath.Join(f.host.SysNVMe, "nvme1", "nvme0c1n1", "ana_state"), []byte("inaccessible\n"), 0o644)
	if abnormal, msg = f.host.pathCondition(cfg); !abnormal || !strings.Contains(msg, "1 of 2 paths usable") || !strings.Contains(msg, "live inaccessible") {
		t.Fatal(abnormal, msg)
	}
	if err := f.host.rescan(cfg.NQN); err != nil {
		t.Fatal(err)
	}
	if readAttr(filepath.Join(f.host.SysNVMe, "nvme0", "rescan_controller")) != "1" {
		t.Error("live path not rescanned")
	}
	if err := f.host.disconnect(cfg.NQN); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"nvme0", "nvme1"} {
		if readAttr(filepath.Join(f.host.SysNVMe, c, "delete_controller")) != "1" {
			t.Errorf("%s not deleted", c)
		}
	}
}

func TestNVMeHostToleratesOneDeadPortalButNotAll(t *testing.T) {
	f := newFakeNVMeSys(t)
	f.refuse["10.0.0.2:4420"] = true
	cfg := nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "v", Portals: []string{"10.0.0.1:4420", "10.0.0.2:4420"}}
	if _, err := f.host.connect(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	abnormal, msg := f.host.pathCondition(cfg)
	if !abnormal || !strings.Contains(msg, "tcp 10.0.0.2:4420 not connected") {
		t.Fatal(abnormal, msg)
	}
	g := newFakeNVMeSys(t)
	g.refuse["10.0.0.1:4420"] = true
	if _, err := g.host.connect(context.Background(), nvmeConfig{Transport: transportNVMeTCP, NQN: cfg.NQN, Portals: cfg.Portals[:1]}); err == nil {
		t.Fatal("no path at all succeeded")
	}
}

func TestNVMeHostRefusesSeveralPathsWithoutNativeMultipath(t *testing.T) {
	f := newFakeNVMeSys(t)
	_ = os.WriteFile(f.host.MultipathFile, []byte("N\n"), 0o644)
	_, err := f.host.connect(context.Background(), nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "v", Portals: []string{"1.1.1.1:1", "2.2.2.2:2"}})
	if err == nil || !strings.Contains(err.Error(), "multipath") {
		t.Fatal(err)
	}
}

func TestHostNQNPrefersTheConfiguredOne(t *testing.T) {
	f := newFakeNVMeSys(t)
	_ = os.WriteFile(f.host.HostNQNFile, []byte("nqn.2014-08.org.nvmexpress:uuid:configured\n"), 0o644)
	if got, _ := f.host.hostNQN(); got != "nqn.2014-08.org.nvmexpress:uuid:configured" {
		t.Fatal(got)
	}
	if got := f.host.hostID("nqn.2014-08.org.nvmexpress:uuid:configured"); !uuidPattern.MatchString(got) {
		t.Fatalf("hostid %q", got)
	}
	_ = os.WriteFile(f.host.HostIDFile, []byte("11111111-2222-3333-4444-555555555555\n"), 0o644)
	if got := f.host.hostID("nqn.2014-08.org.nvmexpress:uuid:01234567-89ab-cdef-0123-456789abcdef"); got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("hostid file ignored: %q", got)
	}
	_ = os.Remove(f.host.HostNQNFile)
	_ = os.WriteFile(f.host.MachineIDFile, []byte("short"), 0o644)
	if _, err := f.host.hostNQN(); err == nil {
		t.Fatal("bad machine id accepted")
	}
}

func blockCap() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func TestNodeStagesAnNVMeVolumeFromItsHandleAlone(t *testing.T) {
	f := newTestNodeServer(t)
	sys := newFakeNVMeSys(t)
	f.server.nvme = sys.host
	f.server.isDeviceFn = func(p string) bool { return p == "/dev/nvme0n1" }
	cfg := nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "pvc-static", Portals: []string{"10.0.0.1:4420"}}
	staging := filepath.Join(t.TempDir(), "staging")
	if _, err := f.server.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: encodeNVMeVolumeID(cfg), StagingTargetPath: staging, VolumeCapability: blockCap(),
	}); err != nil {
		t.Fatal(err)
	}
	if len(sys.connects) != 1 || !strings.Contains(sys.connects[0], "nqn="+cfg.NQN+",transport=tcp,traddr=10.0.0.1,trsvcid=4420") {
		t.Fatalf("%q", sys.connects)
	}
}

func TestNodeStagesAndPublishesAnNVMeBlockVolume(t *testing.T) {
	f := newTestNodeServer(t)
	sys := newFakeNVMeSys(t)
	f.server.nvme = sys.host
	devices := map[string]bool{"/dev/nvme0n1": true}
	f.server.isDeviceFn = func(p string) bool { return devices[p] }
	f.server.blockSizeFn = func(string) (int64, error) { return 8 << 30, nil }
	var mountedFrom string
	f.server.mountFn = func(source, target, fsType string, flags uintptr, data string) error {
		f.mounted[target] = true
		mountedFrom = source
		devices[target] = true
		return nil
	}
	cfg := nvmeConfig{Transport: transportNVMeTCP, NQN: nqnPrefix + "pvc-b", Portals: []string{"10.0.0.1:4420", "10.0.0.2:4420"}}
	id := encodeNVMeVolumeID(cfg)
	ctx := context.Background()
	staging := filepath.Join(t.TempDir(), "staging")
	vc := map[string]string{volumeAttrTransport: "nvme-tcp", volumeAttrNQN: cfg.NQN, volumeAttrPortals: strings.Join(cfg.Portals, ",")}
	if _, err := f.server.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: id, StagingTargetPath: staging, VolumeCapability: blockCap(), VolumeContext: vc}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(StagedDevicePath(staging)); got != "/dev/nvme0n1" {
		t.Fatalf("staged device %q", got)
	}
	if len(f.run.calls) != 0 {
		t.Fatalf("block staging ran commands (mkfs?): %v", f.run.calls)
	}
	if _, err := f.server.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{VolumeId: id, StagingTargetPath: staging, VolumeCapability: blockCap(), VolumeContext: vc}); err != nil || len(sys.connects) != 2 {
		t.Fatalf("re-stage reconnected: %v %d", err, len(sys.connects))
	}

	// Publish resolves the symlink itself; a fake /dev path can't be
	// EvalSymlink'd, so point the link at a real file standing in for it.
	devDir, _ := filepath.EvalSymlinks(t.TempDir())
	fakeDev := filepath.Join(devDir, "nvme0n1")
	_ = os.WriteFile(fakeDev, nil, 0o600)
	_ = os.Remove(StagedDevicePath(staging))
	_ = os.Symlink(fakeDev, StagedDevicePath(staging))
	target := filepath.Join(t.TempDir(), "publish", "pvc-b")
	if _, err := f.server.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: id, StagingTargetPath: staging, TargetPath: target, VolumeCapability: blockCap()}); err != nil {
		t.Fatal(err)
	}
	if mountedFrom != fakeDev {
		t.Fatalf("bind source %q", mountedFrom)
	}
	if fi, err := os.Stat(target); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("publish target is not a file: %v", err)
	}

	stats, err := f.server.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: id, VolumePath: target})
	if err != nil {
		t.Fatal(err)
	}
	if stats.GetUsage()[0].GetTotal() != 8<<30 {
		t.Fatalf("%+v", stats)
	}

	if _, err := f.server.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: id, TargetPath: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("block target file left behind")
	}
	if _, err := f.server.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: id, StagingTargetPath: staging}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(StagedDevicePath(staging)); !os.IsNotExist(err) {
		t.Error("device link left behind")
	}
	if readAttr(filepath.Join(sys.host.SysNVMe, "nvme0", "delete_controller")) != "1" {
		t.Error("controller not disconnected")
	}
}

func TestNodeStagesAnISCSIBlockVolumeWithoutFormatting(t *testing.T) {
	f := newTestNodeServer(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	dev := filepath.Join(dir, "sdz")
	_ = os.WriteFile(dev, nil, 0o600)
	link := filepath.Join(dir, "by-path")
	_ = os.Symlink(dev, link)
	orig := devicePathFunc
	devicePathFunc = func(iscsiConfig) string { return link }
	t.Cleanup(func() { devicePathFunc = orig })
	f.run.on("iscsiadm", func(args ...string) (string, error) { return "", nil })
	staging := filepath.Join(dir, "staging")
	_, err := f.server.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: "iscsi|10.0.0.1:3260|iqn.x|0", StagingTargetPath: staging, VolumeCapability: blockCap(),
		VolumeContext: map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(StagedDevicePath(staging)); got != dev {
		t.Fatalf("staged %q", got)
	}
	if len(f.run.callsFor("blkid")) != 0 || len(f.run.callsFor("mkfs.ext4")) != 0 {
		t.Fatal("block volume was probed or formatted")
	}
}

func TestControllerProvisionsNVMeVolumesOnEveryPortalOfTheTransport(t *testing.T) {
	s := newTestControllerServer(t, newFakeCommandRunner())
	c := newTestNVMet(t)
	portals, _ := ParseNVMePortals("tcp:10.0.0.1:4420,rdma:10.0.0.1:4421,tcp:10.0.0.2:4420")
	if err := s.EnableNVMe(portals, c.Root); err != nil {
		t.Fatal(err)
	}
	s.nvmet.rmdir = os.RemoveAll
	ctx := context.Background()
	resp, err := s.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-n", VolumeCapabilities: []*csi.VolumeCapability{blockCap()},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 20},
		Parameters:    map[string]string{volumeAttrTransport: "nvme-tcp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := resp.GetVolume()
	if v.GetVolumeId() != "nvme|nvme-tcp|"+nqnPrefix+"pvc-n|10.0.0.1:4420,10.0.0.2:4420" {
		t.Fatal(v.GetVolumeId())
	}
	if v.GetVolumeContext()[volumeAttrPortals] != "10.0.0.1:4420,10.0.0.2:4420" {
		t.Fatal(v.GetVolumeContext())
	}
	nqn := nqnPrefix + "pvc-n"
	for _, id := range []int{nvmetPortIDBase, nvmetPortIDBase + 2} {
		if _, err := os.Lstat(filepath.Join(c.portDir(id), "subsystems", nqn)); err != nil {
			t.Errorf("port %d not linked: %v", id, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(c.portDir(nvmetPortIDBase+1), "subsystems", nqn)); err == nil {
		t.Error("the rdma portal was linked for an nvme-tcp volume")
	}
	if fi, err := os.Stat(s.backingFilePath("pvc-n")); err != nil || fi.Size() != 2<<20 {
		t.Fatalf("backing file: %v", err)
	}

	exp, err := s.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{VolumeId: v.GetVolumeId(), CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 20}})
	if err != nil || exp.GetCapacityBytes() != 4<<20 {
		t.Fatal(exp, err)
	}
	if readAttr(filepath.Join(c.subsystemDir(nqn), "namespaces", "1", "revalidate_size")) != "1" {
		t.Error("size not revalidated")
	}
	snap, err := s.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{Name: "snap-n", SourceVolumeId: v.GetVolumeId()})
	if err != nil || snap.GetSnapshot().GetSizeBytes() != 4<<20 {
		t.Fatal(snap, err)
	}

	if _, err := s.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: v.GetVolumeId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.subsystemDir(nqn)); !os.IsNotExist(err) {
		t.Error("subsystem not deleted")
	}
	if _, err := os.Stat(s.backingFilePath("pvc-n")); !os.IsNotExist(err) {
		t.Error("backing file not deleted")
	}

	if _, err := s.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-x", VolumeCapabilities: []*csi.VolumeCapability{blockCap()},
		Parameters: map[string]string{volumeAttrTransport: "nvme-fc"},
	}); err == nil {
		t.Fatal("unknown transport accepted")
	}
	plain := newTestControllerServer(t, newFakeCommandRunner())
	if _, err := plain.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-y", VolumeCapabilities: []*csi.VolumeCapability{blockCap()},
		Parameters: map[string]string{volumeAttrTransport: "nvme-tcp"},
	}); err == nil {
		t.Fatal("nvme volume on a controller without portals accepted")
	}
}
