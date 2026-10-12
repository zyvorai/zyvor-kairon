// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/csinode"
	"github.com/zyvorai/kairon/internal/model"
)

func TestParseThirdPartyCSIDrivers(t *testing.T) {
	got, err := ParseThirdPartyCSIDrivers("rbd.csi.ceph.com=/var/lib/kubelet/plugins/rbd.csi.ceph.com/csi.sock, other.example.com=/tmp/other.sock ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"rbd.csi.ceph.com":  "/var/lib/kubelet/plugins/rbd.csi.ceph.com/csi.sock",
		"other.example.com": "/tmp/other.sock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseThirdPartyCSIDriversEmptyReturnsEmptyMap(t *testing.T) {
	got, err := ParseThirdPartyCSIDrivers("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestParseThirdPartyCSIDriversRejectsMalformedEntries(t *testing.T) {
	for _, bad := range []string{"no-equals-sign", "=/no/driver/name", "driver.example.com="} {
		if _, err := ParseThirdPartyCSIDrivers(bad); err == nil {
			t.Errorf("ParseThirdPartyCSIDrivers(%q): expected an error", bad)
		}
	}
}

func testThirdPartyCSIPV(name, driver, volumeHandle string) model.PersistentVolume {
	return model.PersistentVolume{
		Metadata: model.ObjectMeta{Name: name},
		Spec: model.PersistentVolumeSpec{
			CSI: &model.CSIPersistentVolumeSource{
				Driver:           driver,
				VolumeHandle:     volumeHandle,
				FSType:           "ext4",
				VolumeAttributes: map[string]string{"pool": "rbd"},
			},
		},
	}
}

func TestResolveCSIVolumeRoutesToThirdPartyDriverWhenAllowlisted(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{
		CSIStagingDir:        t.TempDir(),
		CSIPublishDir:        t.TempDir(),
		ThirdPartyCSIDrivers: map[string]string{"rbd.csi.ceph.com": socketPath},
	}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", "rbd.csi.ceph.com", "rbd-image-123")

	path, volStatus, err := a.resolveCSIVolume(context.Background(), m, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	wantPath := filepath.Join(a.CSIPublishDir, "thirdparty", "rbd.csi.ceph.com", m.RuntimeName(), bootDiskFileName)
	if path != wantPath {
		t.Fatalf("got path %q, want %q", path, wantPath)
	}
	if volStatus.Driver != "rbd.csi.ceph.com" || volStatus.VolumeID != "rbd-image-123" {
		t.Fatalf("unexpected volStatus: %+v", volStatus)
	}
	if len(fake.stageCalls) != 1 || len(fake.publishCalls) != 1 {
		t.Fatalf("expected exactly one stage and one publish call, got %d/%d", len(fake.stageCalls), len(fake.publishCalls))
	}
	if fake.stageCalls[0].GetVolumeContext()["pool"] != "rbd" {
		t.Fatalf("expected volume_context to carry the PV's volumeAttributes, got %+v", fake.stageCalls[0].GetVolumeContext())
	}
	if len(fake.stageCalls[0].GetSecrets()) != 0 {
		t.Fatal("expected no secrets when the PV has no secret refs")
	}
}

func TestResolveCSIVolumeThirdPartyBlockModePublishesDevice(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{
		CSIStagingDir:        t.TempDir(),
		CSIPublishDir:        t.TempDir(),
		ThirdPartyCSIDrivers: map[string]string{"rbd.csi.ceph.com": socketPath},
	}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", "rbd.csi.ceph.com", "rbd-image-123")
	pv.Spec.VolumeMode = "Block"

	path, _, err := a.resolveCSIVolume(context.Background(), m, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	wantPath := filepath.Join(a.CSIPublishDir, "thirdparty", "rbd.csi.ceph.com", m.RuntimeName())
	if path != wantPath {
		t.Fatalf("got path %q, want the publish target itself %q", path, wantPath)
	}
	if fake.stageCalls[0].GetVolumeCapability().GetBlock() == nil || fake.publishCalls[0].GetVolumeCapability().GetBlock() == nil {
		t.Fatal("expected a Block volume capability on stage and publish")
	}
	if fi, err := os.Stat(filepath.Dir(wantPath)); err != nil || !fi.IsDir() {
		t.Fatalf("expected the publish parent directory to exist: %v", err)
	}

	m.Status = model.MachineStatus{VolumeStagingPath: "/s", VolumePublishPath: "/p", VolumeHandle: "rbd-image-123", VolumeDriver: "rbd.csi.ceph.com"}
	if path, _, err = a.resolveCSIVolume(context.Background(), m, pv); err != nil || path != "/p" {
		t.Fatalf("reused block path = %q, %v; want /p", path, err)
	}
}

func TestResolveCSIVolumeOwnDriverAcceptsBlockMode(t *testing.T) {
	a := &Agent{CSIStagingDir: t.TempDir(), CSIPublishDir: t.TempDir()}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", csinode.DriverName, "iqn")
	pv.Spec.VolumeMode = "Block"
	// Block mode gets as far as the node plugin, which this agent lacks.
	if _, _, err := a.resolveCSIVolume(context.Background(), m, pv); err == nil || strings.Contains(err.Error(), "Block-mode") || !strings.Contains(err.Error(), "no CSI socket") {
		t.Fatalf("expected only the missing-socket error, got %v", err)
	}
}

func TestResolveCSIVolumeRejectsDriverNotInThirdPartyAllowlist(t *testing.T) {
	a := &Agent{CSIStagingDir: t.TempDir(), CSIPublishDir: t.TempDir(), ThirdPartyCSIDrivers: map[string]string{"other.example.com": "/unused"}}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	pv := testThirdPartyCSIPV("pv-1", "rbd.csi.ceph.com", "rbd-image-123")

	if _, _, err := a.resolveCSIVolume(context.Background(), m, pv); err == nil {
		t.Fatal("expected an error for a driver not present in the third-party allowlist -- fail closed")
	}
}

func TestResolveCSIVolumeThirdPartyIsIdempotentAgainstExistingStatus(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{
		CSIStagingDir:        t.TempDir(),
		CSIPublishDir:        t.TempDir(),
		ThirdPartyCSIDrivers: map[string]string{"rbd.csi.ceph.com": socketPath},
	}
	pv := testThirdPartyCSIPV("pv-1", "rbd.csi.ceph.com", "rbd-image-123")
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Status: model.MachineStatus{
			VolumeStagingPath: "/already/staged", VolumePublishPath: "/already/published",
			VolumeHandle: "rbd-image-123", VolumeDriver: "rbd.csi.ceph.com",
		},
	}

	path, volStatus, err := a.resolveCSIVolume(context.Background(), m, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	if path != filepath.Join("/already/published", bootDiskFileName) {
		t.Fatalf("expected the already-published path to be reused, got %q", path)
	}
	if volStatus.Driver != "rbd.csi.ceph.com" {
		t.Fatalf("unexpected volStatus: %+v", volStatus)
	}
	if len(fake.stageCalls) != 0 || len(fake.publishCalls) != 0 {
		t.Fatal("expected no gRPC calls when status already records a matching staged/published volume")
	}
}

func TestTeardownCSIVolumeRoutesToThirdPartyDriver(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{ThirdPartyCSIDrivers: map[string]string{"rbd.csi.ceph.com": socketPath}}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Status: model.MachineStatus{
			VolumeStagingPath: "/staged", VolumePublishPath: "/published",
			VolumeHandle: "rbd-image-123", VolumeDriver: "rbd.csi.ceph.com",
		},
	}
	if err := a.teardownCSIVolume(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(fake.unpublish) != 1 || len(fake.unstage) != 1 {
		t.Fatalf("expected exactly one unpublish and one unstage call, got %d/%d", len(fake.unpublish), len(fake.unstage))
	}
	if fake.unpublish[0].GetVolumeId() != "rbd-image-123" {
		t.Fatalf("unexpected unpublish request: %+v", fake.unpublish[0])
	}
}

func TestPruneStaleCSIVolumeRoutesToThirdPartyDriverForTheOldVolume(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{ThirdPartyCSIDrivers: map[string]string{"rbd.csi.ceph.com": socketPath}}
	// Simulates editing spec.volumes[0] away from an RBD-backed PVC to a
	// Kairon-driver-backed one -- the OLD status names the third-party
	// driver; pruneStaleCSIVolume must route the OLD volume's teardown to
	// it, not to Kairon's own (unconfigured here) socket.
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Status: model.MachineStatus{
			VolumeStagingPath: "/staged", VolumePublishPath: "/published",
			VolumeHandle: "rbd-image-123", VolumeDriver: "rbd.csi.ceph.com",
		},
	}
	next := csiVolumeStatus{VolumeID: "iscsi|new|i|0"} // Driver == "" -- Kairon's own driver now
	if err := a.pruneStaleCSIVolume(context.Background(), m, next); err != nil {
		t.Fatalf("pruneStaleCSIVolume: %v", err)
	}
	if len(fake.unpublish) != 1 || fake.unpublish[0].GetVolumeId() != "rbd-image-123" {
		t.Fatalf("expected the old RBD volume to be torn down via its own driver socket, got %+v", fake.unpublish)
	}
}

func TestTeardownCSIVolumeStillRoutesToOwnDriverWhenVolumeDriverEmpty(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"},
		Status: model.MachineStatus{
			VolumeStagingPath: "/staged", VolumePublishPath: "/published",
			VolumeHandle: "vol-1", // VolumeDriver left empty -- backward compatibility with Machines created before this field existed
		},
	}
	if err := a.teardownCSIVolume(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(fake.unpublish) != 1 || len(fake.unstage) != 1 {
		t.Fatalf("expected exactly one unpublish and one unstage call, got %d/%d", len(fake.unpublish), len(fake.unstage))
	}
}
