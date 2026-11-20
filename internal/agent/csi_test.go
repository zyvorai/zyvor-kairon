// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/zyvorai/kairon/internal/csinode"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

// shortTempDir mirrors internal/consoleproxy's/uiapi's own helper of the
// same name: AF_UNIX socket paths are capped at ~104 bytes on macOS, well
// under what t.TempDir()'s test-name-based nesting produces.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "csi")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeCSINodeServer is a minimal in-process double for kairon-csi-node's
// gRPC surface -- real network I/O over a real Unix socket (not a hand-
// rolled Agent-side mock), so this exercises the actual dial/call path
// kairon-node uses in production, just against a fake instead of
// internal/csinode's real iSCSI-backed implementation (which needs a
// real target -- see that package's own tests for its orchestration
// logic).
type fakeCSINodeServer struct {
	csi.UnimplementedNodeServer
	stageCalls   []*csi.NodeStageVolumeRequest
	publishCalls []*csi.NodePublishVolumeRequest
	unpublish    []*csi.NodeUnpublishVolumeRequest
	unstage      []*csi.NodeUnstageVolumeRequest
	failStage    bool
}

func (f *fakeCSINodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if f.failStage {
		return nil, context.DeadlineExceeded
	}
	f.stageCalls = append(f.stageCalls, req)
	return &csi.NodeStageVolumeResponse{}, nil
}

func (f *fakeCSINodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	f.publishCalls = append(f.publishCalls, req)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (f *fakeCSINodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	f.unpublish = append(f.unpublish, req)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (f *fakeCSINodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	f.unstage = append(f.unstage, req)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// startFakeCSINode listens on a Unix socket under t.TempDir() and serves
// fake off it, returning the socket path (what Agent.CSISocketPath
// expects) and a cleanup already registered via t.Cleanup.
func startFakeCSINode(t *testing.T, fake *fakeCSINodeServer) string {
	t.Helper()
	socketPath := filepath.Join(shortTempDir(t), "csi.sock")
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := grpc.NewServer()
	csi.RegisterNodeServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return socketPath
}

func testCSIPV(name, volumeHandle string) model.PersistentVolume {
	return model.PersistentVolume{
		Metadata: model.ObjectMeta{Name: name},
		Spec: model.PersistentVolumeSpec{
			CSI: &model.CSIPersistentVolumeSource{
				Driver:           csinode.DriverName,
				VolumeHandle:     volumeHandle,
				FSType:           "ext4",
				VolumeAttributes: map[string]string{"portal": "10.0.0.1:3260", "iqn": "iqn.test:disk"},
			},
		},
	}
}

func TestResolveCSIVolumeStagesAndPublishes(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath, CSIStagingDir: t.TempDir(), CSIPublishDir: t.TempDir()}

	pv := testCSIPV("pv-1", "iscsi|10.0.0.1:3260|iqn.test:disk|0")
	path, volStatus, err := a.resolveCSIVolume(context.Background(), model.Machine{}, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	want := filepath.Join(a.CSIPublishDir, "pv-1", bootDiskFileName)
	if path != want {
		t.Fatalf("got path %q, want %q", path, want)
	}
	if volStatus.VolumeID != pv.Spec.CSI.VolumeHandle {
		t.Fatalf("got volume ID %q, want %q", volStatus.VolumeID, pv.Spec.CSI.VolumeHandle)
	}
	if len(fake.stageCalls) != 1 || len(fake.publishCalls) != 1 {
		t.Fatalf("expected exactly one stage and one publish call, got %d/%d", len(fake.stageCalls), len(fake.publishCalls))
	}
	if fake.stageCalls[0].GetVolumeContext()["portal"] != "10.0.0.1:3260" {
		t.Fatalf("expected volume_context to carry the PV's volumeAttributes, got %+v", fake.stageCalls[0].GetVolumeContext())
	}
	if len(fake.stageCalls[0].GetSecrets()) != 0 {
		t.Fatal("expected no secrets to be sent when the PV has no nodeStageSecretRef")
	}
}

func TestResolveCSIVolumePassesChapSecretsFromAllowlistedNamespace(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/kairon-system/secrets/iscsi-chap" {
			_ = json.NewEncoder(w).Encode(model.Secret{
				Metadata: model.ObjectMeta{Name: "iscsi-chap", Namespace: "kairon-system"},
				Data: map[string][]byte{
					csinode.SecretKeyUsername: []byte("alice"),
					csinode.SecretKeyPassword: []byte("s3cret"),
				},
			})
			return
		}
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()
	kc, _ := kube.New(srv.URL, "", "", false)
	kc.HTTP = srv.Client()

	a := &Agent{
		Kube:                   kc,
		CSISocketPath:          socketPath,
		CSIStagingDir:          t.TempDir(),
		CSIPublishDir:          t.TempDir(),
		CSIChapSecretNamespace: "kairon-system",
	}
	pv := testCSIPV("pv-1", "iscsi|10.0.0.1:3260|iqn.test:disk|0")
	pv.Spec.CSI.NodeStageSecretRef = &model.SecretReference{Name: "iscsi-chap"}

	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{}, pv); err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	if len(fake.stageCalls) != 1 {
		t.Fatalf("expected one stage call, got %d", len(fake.stageCalls))
	}
	got := fake.stageCalls[0].GetSecrets()
	if got[csinode.SecretKeyUsername] != "alice" || got[csinode.SecretKeyPassword] != "s3cret" {
		t.Fatalf("expected CHAP secrets on NodeStageVolume, got %+v", got)
	}
}

func TestResolveCSIVolumeRefusesChapWithoutAllowlist(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath, CSIStagingDir: t.TempDir(), CSIPublishDir: t.TempDir()}
	pv := testCSIPV("pv-1", "iscsi|10.0.0.1:3260|iqn.test:disk|0")
	pv.Spec.CSI.NodeStageSecretRef = &model.SecretReference{Name: "iscsi-chap"}
	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{}, pv); err == nil {
		t.Fatal("expected refusal when nodeStageSecretRef is set without CSIChapSecretNamespace")
	}
	if len(fake.stageCalls) != 0 {
		t.Fatal("expected no stage call when CHAP resolution fails closed")
	}
}

func TestResolveCSIVolumeIsIdempotentAgainstExistingStatus(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath, CSIStagingDir: t.TempDir(), CSIPublishDir: t.TempDir()}

	pv := testCSIPV("pv-1", "iscsi|10.0.0.1:3260|iqn.test:disk|0")
	status := model.MachineStatus{
		VolumeStagingPath: "/already/staged", VolumePublishPath: "/already/published", VolumeHandle: pv.Spec.CSI.VolumeHandle,
	}
	path, volStatus, err := a.resolveCSIVolume(context.Background(), model.Machine{Status: status}, pv)
	if err != nil {
		t.Fatalf("resolveCSIVolume: %v", err)
	}
	if path != filepath.Join("/already/published", bootDiskFileName) {
		t.Fatalf("expected the already-published path to be reused, got %q", path)
	}
	if volStatus.StagingPath != "/already/staged" {
		t.Fatalf("expected the cached staging path, got %q", volStatus.StagingPath)
	}
	if len(fake.stageCalls) != 0 || len(fake.publishCalls) != 0 {
		t.Fatal("expected no gRPC calls when status already records a matching staged/published volume")
	}
}

func TestResolveCSIVolumeRejectsUnknownDriver(t *testing.T) {
	a := &Agent{CSISocketPath: "/unused", CSIStagingDir: "/x", CSIPublishDir: "/y"}
	pv := model.PersistentVolume{Spec: model.PersistentVolumeSpec{CSI: &model.CSIPersistentVolumeSource{Driver: "some-other-driver.example.com"}}}
	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{}, pv); err == nil {
		t.Fatal("expected an error for a CSI driver that isn't Kairon's own or listed in the third-party allowlist")
	}
}

func TestResolveCSIVolumeRequiresSocketConfigured(t *testing.T) {
	a := &Agent{CSIStagingDir: "/x", CSIPublishDir: "/y"} // CSISocketPath unset
	pv := testCSIPV("pv-1", "iscsi|p|i|0")
	if _, _, err := a.resolveCSIVolume(context.Background(), model.Machine{}, pv); err == nil {
		t.Fatal("expected an error when no CSI socket is configured")
	}
}

func TestTeardownCSIVolumeUnpublishesAndUnstages(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath}

	m := model.Machine{
		Status: model.MachineStatus{
			VolumeStagingPath: "/stage/pv-1", VolumePublishPath: "/publish/pv-1", VolumeHandle: "iscsi|p|i|0",
		},
	}
	if err := a.teardownCSIVolume(context.Background(), m); err != nil {
		t.Fatalf("teardownCSIVolume: %v", err)
	}
	if len(fake.unpublish) != 1 || fake.unpublish[0].GetTargetPath() != "/publish/pv-1" {
		t.Fatalf("expected one NodeUnpublishVolume call against /publish/pv-1, got %+v", fake.unpublish)
	}
	if len(fake.unstage) != 1 || fake.unstage[0].GetStagingTargetPath() != "/stage/pv-1" {
		t.Fatalf("expected one NodeUnstageVolume call against /stage/pv-1, got %+v", fake.unstage)
	}
}

func TestTeardownCSIVolumeNoOpsWhenNeverStaged(t *testing.T) {
	// No CSISocketPath configured at all -- if this tried to dial, it
	// would fail loudly (see TestResolveCSIVolumeRequiresSocketConfigured);
	// succeeding here proves the no-op path never even attempts to dial.
	a := &Agent{}
	if err := a.teardownCSIVolume(context.Background(), model.Machine{}); err != nil {
		t.Fatalf("expected a no-op for a Machine that never staged a CSI volume, got: %v", err)
	}
}

func TestPruneStaleCSIVolumeNoOpWhenNothingStagedYet(t *testing.T) {
	// No CSISocketPath either -- proves this never even attempts to dial
	// when m.Status has no staged/published volume to begin with (the
	// overwhelmingly common first-reconcile and non-CSI cases).
	a := &Agent{}
	if err := a.pruneStaleCSIVolume(context.Background(), model.Machine{}, csiVolumeStatus{VolumeID: "iscsi|p|i|0"}); err != nil {
		t.Fatalf("expected a no-op, got: %v", err)
	}
}

func TestPruneStaleCSIVolumeNoOpWhenUnchanged(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath}

	m := model.Machine{Status: model.MachineStatus{
		VolumeStagingPath: "/stage/pv-1", VolumePublishPath: "/publish/pv-1", VolumeHandle: "iscsi|p|i|0",
	}}
	// Same VolumeID/Driver resolveCSIVolume's own idempotent path would
	// return unchanged -- must not redundantly unpublish/unstage the
	// volume the Machine is still actively using.
	next := csiVolumeStatus{StagingPath: "/stage/pv-1", PublishPath: "/publish/pv-1", VolumeID: "iscsi|p|i|0"}
	if err := a.pruneStaleCSIVolume(context.Background(), m, next); err != nil {
		t.Fatalf("pruneStaleCSIVolume: %v", err)
	}
	if len(fake.unpublish) != 0 || len(fake.unstage) != 0 {
		t.Fatalf("expected no teardown calls for an unchanged volume, got unpublish=%d unstage=%d", len(fake.unpublish), len(fake.unstage))
	}
}

func TestPruneStaleCSIVolumeTearsDownOldVolumeOnHandleChange(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath}

	// Simulates editing spec.volumes[0].claimName to point at a different
	// PVC/PV -- the newly resolved volume (next) has a different handle
	// than what m.Status still records from a previous tick.
	m := model.Machine{Status: model.MachineStatus{
		VolumeStagingPath: "/stage/old", VolumePublishPath: "/publish/old", VolumeHandle: "iscsi|old|i|0",
	}}
	next := csiVolumeStatus{StagingPath: "/stage/new", PublishPath: "/publish/new", VolumeID: "iscsi|new|i|0"}
	if err := a.pruneStaleCSIVolume(context.Background(), m, next); err != nil {
		t.Fatalf("pruneStaleCSIVolume: %v", err)
	}
	if len(fake.unpublish) != 1 || fake.unpublish[0].GetTargetPath() != "/publish/old" {
		t.Fatalf("expected the OLD volume's publish path to be torn down, got %+v", fake.unpublish)
	}
	if len(fake.unstage) != 1 || fake.unstage[0].GetStagingTargetPath() != "/stage/old" {
		t.Fatalf("expected the OLD volume's staging path to be torn down, got %+v", fake.unstage)
	}
}

func TestPruneStaleCSIVolumeTearsDownOldVolumeWhenSpecVolumesRemoved(t *testing.T) {
	fake := &fakeCSINodeServer{}
	socketPath := startFakeCSINode(t, fake)
	a := &Agent{CSISocketPath: socketPath}

	// Simulates removing spec.volumes entirely (falling back to plain
	// spec.image.path) -- reconcileMachine's own volStatus for that path
	// is the zero value, matching nothing that was ever staged.
	m := model.Machine{Status: model.MachineStatus{
		VolumeStagingPath: "/stage/old", VolumePublishPath: "/publish/old", VolumeHandle: "iscsi|old|i|0",
	}}
	if err := a.pruneStaleCSIVolume(context.Background(), m, csiVolumeStatus{}); err != nil {
		t.Fatalf("pruneStaleCSIVolume: %v", err)
	}
	if len(fake.unpublish) != 1 || len(fake.unstage) != 1 {
		t.Fatalf("expected the abandoned volume to be torn down, got unpublish=%d unstage=%d", len(fake.unpublish), len(fake.unstage))
	}
}

func TestPruneStaleCSIVolumeFailsClosedOnTeardownError(t *testing.T) {
	// No CSISocketPath configured -- teardownCSIVolume's dial/call will
	// fail, and that failure must propagate rather than being swallowed,
	// so reconcileMachine's caller never commits the new volStatus over a
	// volume that's still actually attached.
	a := &Agent{}
	m := model.Machine{Status: model.MachineStatus{
		VolumeStagingPath: "/stage/old", VolumePublishPath: "/publish/old", VolumeHandle: "iscsi|old|i|0",
	}}
	next := csiVolumeStatus{VolumeID: "iscsi|new|i|0"}
	if err := a.pruneStaleCSIVolume(context.Background(), m, next); err == nil {
		t.Fatal("expected pruneStaleCSIVolume to fail closed when the old volume's teardown itself errors")
	}
}
