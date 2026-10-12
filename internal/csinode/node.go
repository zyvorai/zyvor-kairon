// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DriverName identifies this CSI driver -- the value a statically
// provisioned PersistentVolume's spec.csi.driver must match exactly, and
// what this reports back via GetPluginInfo/NodeGetInfo. Domain-shaped per
// the CSI spec's own naming convention.
const DriverName = "csi.kairon.zyvor.dev"

// stagingDirPerm/publishDirPerm: kairon-csi-node runs privileged (it has
// to, to log in to iSCSI targets and mount block devices), so these
// directories are created 0750 rather than world-readable -- there is no
// legitimate reason for anything other than root (this plugin) and the
// group it runs as to read a Machine's raw disk image directory.
const stagingDirPerm = 0o750

// stagedDeviceLink is the symlink a Block volume's staging directory holds,
// pointing at the real /dev node. Publish bind-mounts that device, and
// kairon-node reads the link to hand FluxVM a /dev path.
const stagedDeviceLink = "device"

// StagedDevicePath is where a Block volume staged at stagingPath records
// its device.
func StagedDevicePath(stagingPath string) string {
	return filepath.Join(stagingPath, stagedDeviceLink)
}

// NodeServer implements the CSI Node service for Kairon's first-cut
// network-block driver, iSCSI or NVMe-oF, Filesystem or Block mode -- see
// docs/guides/machine-storage-csi.md for the full design, including why
// this breaks Go-stdlib-only (a real iSCSI initiator and filesystem
// tooling aren't hand-rolled) and its current, real limits, and
// docs/guides/machine-storage-nvmeof.md for NVMe-oF.
//
// A PersistentVolume this driver serves can come from either of two
// places now: an admin still can create one statically (spec.csi.
// volumeAttributes carrying the iSCSI portal/IQN/LUN by hand -- the
// original, still fully supported "pre-provisioned" path), or a
// StorageClass naming this driver as its provisioner can request one
// dynamically via ControllerServer (controller.go), served by the
// separate kairon-csi-controller binary/Deployment -- see its own doc
// comment for what that first cut does and doesn't cover. Either way, the
// Node service's own job is unchanged: log in to whatever portal/IQN/LUN
// the resulting PersistentVolume's volumeAttributes name and mount it.
// Kairon's own Machine boot-disk consumption path (internal/agent) also
// never goes through kubelet's Pod volume machinery at all -- kairon-node
// dials this same node plugin's Unix socket directly as a CSI client, the
// same way it already reads a hostPath/local PersistentVolume's path
// directly without any Pod involved (see internal/agent/storage.go). The
// upstream csi-node-driver-registrar sidecar this ships with still
// registers the plugin with kubelet, so a real Kubernetes Pod (unrelated
// to Kairon) with a matching PVC/PV can also use it normally.
type NodeServer struct {
	csi.UnimplementedNodeServer
	NodeID string
	Runner CommandRunner
	iscsi  *iscsiClient

	// isMountPoint/mountFn/unmountFn default to the real, OS-touching
	// implementations (IsMountPoint, mount, unmount) via NewNodeServer --
	// overridden in tests so this type's actual request-handling logic
	// (validation, idempotency checks, call sequencing, error
	// propagation) is fully unit-testable without a real filesystem,
	// real mount privileges, or a real iSCSI target. See node_test.go and
	// CommandRunner's own doc comment for why the real OS-level behavior
	// these stand in for can only be verified by running the built
	// kairon-csi-node image for real.
	isMountPoint func(path string) (bool, error)
	mountFn      func(source, target, fsType string, flags uintptr, data string) error
	unmountFn    func(target string) error
	statfsFn     func(path string) (VolumeStats, error)
	blockSizeFn  func(path string) (int64, error)
	isDeviceFn   func(path string) bool

	nvme *nvmeHost
}

func blockSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return f.Seek(0, io.SeekEnd)
}

func isDevice(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeDevice != 0
}

// NewNodeServer builds a NodeServer against the real CommandRunner and
// real OS mount/mountinfo calls. nodeID should uniquely identify this
// Kubernetes Node (its name is the natural choice -- see
// cmd/kairon-csi-node/main.go).
func NewNodeServer(nodeID string, runner CommandRunner) *NodeServer {
	return &NodeServer{
		NodeID: nodeID, Runner: runner, iscsi: &iscsiClient{run: runner},
		isMountPoint: IsMountPoint, mountFn: mount, unmountFn: unmount, statfsFn: statfs,
		blockSizeFn: blockSize, isDeviceFn: isDevice, nvme: newNVMeHost(),
	}
}

func (s *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.NodeID}, nil
}

func (s *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{
				Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
			}}},
			{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{
				Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
			}}},
			{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{
				Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
			}}},
		},
	}, nil
}

// NodeGetVolumeStats reports volume_id's disk usage/capacity by
// statfs(2)-ing volume_path directly (see statfs's own doc comment) --
// kubelet calls this on every CSI volume it manages, to serve
// `kubectl describe pod`'s "Used"/du-free capacity display and
// ephemeral-storage-based eviction/metrics without kubelet itself needing
// to know this driver's on-disk layout. volume_path is whatever the CO
// passed to NodeStageVolume/NodePublishVolume -- either a staging or a
// publish path is valid per the CSI spec, and IsMountPoint's mountinfo
// scan recognizes both (a bind mount is its own distinct mountinfo
// entry). Deliberately fails closed rather than fabricating a number: a
// path that isn't currently mounted gets NotFound, and a real statfs
// error gets Internal -- callers make real capacity decisions off this,
// so a wrong answer is worse than a clear error.
func (s *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	path := req.GetVolumePath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}
	mounted, err := s.isMountPoint(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check volume path: %v", err)
	}
	if !mounted {
		return nil, status.Errorf(codes.NotFound, "volume path %s is not a mounted volume", path)
	}
	resp := &csi.NodeGetVolumeStatsResponse{}
	if s.isDeviceFn(path) {
		size, err := s.blockSizeFn(path)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "size of %s: %v", path, err)
		}
		resp.Usage = []*csi.VolumeUsage{{Unit: csi.VolumeUsage_BYTES, Total: size}}
	} else {
		stats, err := s.statfsFn(path)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "statfs %s: %v", path, err)
		}
		resp.Usage = []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: stats.TotalBytes, Used: stats.UsedBytes, Available: stats.AvailableBytes},
			{Unit: csi.VolumeUsage_INODES, Total: stats.TotalInodes, Used: stats.UsedInodes, Available: stats.AvailableInodes},
		}
	}
	return resp, nil
}

// NodeStageVolume attaches the volume (iSCSI login, or an NVMe-oF connect
// on every portal) and then, for a mount volume, formats it if it has no
// filesystem and mounts it at staging_target_path; for a Block volume it
// records the device in a symlink there instead. Idempotent, per the CSI
// spec's own requirement (see IsMountPoint).
func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	staging := req.GetStagingTargetPath()
	if staging == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path is required")
	}
	block := req.GetVolumeCapability().GetBlock() != nil
	mountCap := req.GetVolumeCapability().GetMount()
	if mountCap == nil && !block {
		return nil, status.Error(codes.InvalidArgument, "volume_capability needs a mount or block access type")
	}
	ctxTransport := req.GetVolumeContext()[volumeAttrTransport]
	// A static PersistentVolume may carry only the volume handle, which
	// names the NVMe-oF target in full.
	ref, idErr := decodeAnyVolumeID(req.GetVolumeId())
	idNVMe := idErr == nil && ref.nvme != nil
	nvme := (ctxTransport != "" && ctxTransport != transportISCSI) || (ctxTransport == "" && idNVMe)
	var iscsiCfg iscsiConfig
	var nvmeCfg nvmeConfig
	var err error
	switch {
	case nvme && ctxTransport == "":
		nvmeCfg = *ref.nvme
	case nvme:
		nvmeCfg, err = parseNVMeConfig(req.GetVolumeContext())
	default:
		iscsiCfg, err = parseISCSIConfig(req.GetVolumeContext(), req.GetSecrets())
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if block {
		if target, err := os.Readlink(StagedDevicePath(staging)); err == nil && s.isDeviceFn(target) {
			return &csi.NodeStageVolumeResponse{}, nil
		}
	} else {
		already, err := s.isMountPoint(staging)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check staging path: %v", err)
		}
		if already {
			return &csi.NodeStageVolumeResponse{}, nil
		}
	}

	var device string
	fsType := defaultFSType
	if nvme {
		if device, err = s.nvme.connect(ctx, nvmeCfg); err != nil {
			return nil, status.Errorf(codes.Internal, "nvme connect for volume %s: %v", req.GetVolumeId(), err)
		}
	} else if device, err = s.iscsi.login(ctx, iscsiCfg); err != nil {
		return nil, status.Errorf(codes.Internal, "iscsi login for volume %s: %v", req.GetVolumeId(), err)
	}
	if err := os.MkdirAll(staging, stagingDirPerm); err != nil {
		return nil, status.Errorf(codes.Internal, "create staging directory: %v", err)
	}
	if block {
		link := StagedDevicePath(staging)
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Internal, "replace %s: %v", link, err)
		}
		if err := os.Symlink(device, link); err != nil {
			return nil, status.Errorf(codes.Internal, "record staged device: %v", err)
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}
	if mountCap.GetFsType() != "" {
		fsType = mountCap.GetFsType()
	}
	if err := ensureFormatted(ctx, s.Runner, device, fsType); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.mountFn(device, staging, fsType, 0, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "mount %s at %s: %v", device, staging, err)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeExpandVolume grows volume_id's on-disk filesystem to fill its
// backing device, after ControllerExpandVolume has already grown the LIO
// backstore itself -- see rescan/growFilesystem's own doc comments for
// why both steps (rescan, then grow) are required and in that order.
// volume_path is either a staging or a publish path per the CSI spec;
// this driver's own staging path is what growFilesystem's own remount
// mechanism (xfs_growfs) needs regardless, since xfs can only grow via
// an already-mounted path.
func (s *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_path is required")
	}
	ref, err := decodeAnyVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var device string
	if ref.nvme != nil {
		if err := s.nvme.rescan(ref.nvme.NQN); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if device = s.nvme.namespaceDevice(ref.nvme.NQN); device == "" {
			return nil, status.Errorf(codes.Internal, "no block device for volume %s", req.GetVolumeId())
		}
	} else {
		if err := s.iscsi.rescan(ctx, *ref.iscsi); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if device, err = filepath.EvalSymlinks(devicePathFunc(*ref.iscsi)); err != nil {
			return nil, status.Errorf(codes.Internal, "resolve device for volume %s: %v", req.GetVolumeId(), err)
		}
	}
	// A Block volume has no filesystem here; the consumer sees the new size.
	if req.GetVolumeCapability().GetBlock() != nil || s.isDeviceFn(req.GetVolumePath()) {
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	if err := growFilesystem(ctx, s.Runner, device, req.GetVolumePath()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeExpandVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts staging_target_path (if mounted) and logs
// out of the iSCSI target volume_id encodes -- see decodeVolumeID for why
// the target has to come from volume_id rather than a request field.
// Idempotent: safe to call against an already-unstaged volume.
func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path is required")
	}
	mounted, err := s.isMountPoint(req.GetStagingTargetPath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check staging path: %v", err)
	}
	if mounted {
		if err := s.unmountFn(req.GetStagingTargetPath()); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", req.GetStagingTargetPath(), err)
		}
	}
	if err := os.Remove(StagedDevicePath(req.GetStagingTargetPath())); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove staged device link: %v", err)
	}
	ref, err := decodeAnyVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if ref.nvme != nil {
		if err := s.nvme.disconnect(ref.nvme.NQN); err != nil {
			return nil, status.Errorf(codes.Internal, "nvme disconnect: %v", err)
		}
		return &csi.NodeUnstageVolumeResponse{}, nil
	}
	if err := s.iscsi.logout(ctx, *ref.iscsi); err != nil {
		return nil, status.Errorf(codes.Internal, "iscsi logout: %v", err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the already-staged volume from
// staging_target_path to target_path -- the per-consumer mount CSI's
// two-phase stage/publish split exists for (multiple pods sharing one
// staged volume on the same node). Kairon itself only ever has one
// consumer per volume (one Machine), but implements the full two-phase
// flow anyway since that's what STAGE_UNSTAGE_VOLUME capability commits
// this plugin to for any CSI-compliant caller, not just kairon-node.
func (s *NodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging_target_path is required (NodeStageVolume must run first)")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	already, err := s.isMountPoint(req.GetTargetPath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check target path: %v", err)
	}
	if already {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	source := req.GetStagingTargetPath()
	if req.GetVolumeCapability().GetBlock() != nil {
		// A Block volume is published as its device node bind-mounted
		// over a file at target_path.
		device, err := filepath.EvalSymlinks(StagedDevicePath(source))
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "volume is not staged as a block device: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(req.GetTargetPath()), stagingDirPerm); err != nil {
			return nil, status.Errorf(codes.Internal, "create target parent: %v", err)
		}
		f, err := os.OpenFile(req.GetTargetPath(), os.O_CREATE|os.O_RDONLY, 0o640)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create target file: %v", err)
		}
		_ = f.Close()
		source = device
	} else if err := os.MkdirAll(req.GetTargetPath(), stagingDirPerm); err != nil {
		return nil, status.Errorf(codes.Internal, "create target directory: %v", err)
	}
	if err := s.mountFn(source, req.GetTargetPath(), "", bindMountFlag, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount %s at %s: %v", source, req.GetTargetPath(), err)
	}
	if req.GetReadonly() {
		// Linux ignores most flags (MS_RDONLY included) on the initial
		// bind-mount call -- a read-only bind mount needs a second,
		// explicit remount, not a single mount() call with both flags
		// set at once.
		if err := s.mountFn("", req.GetTargetPath(), "", bindMountFlag|bindRemountFlag|bindReadOnlyFlag, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "remount %s read-only: %v", req.GetTargetPath(), err)
		}
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts target_path -- idempotent, safe against an
// already-unpublished volume.
func (s *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	mounted, err := s.isMountPoint(req.GetTargetPath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check target path: %v", err)
	}
	if mounted {
		if err := s.unmountFn(req.GetTargetPath()); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", req.GetTargetPath(), err)
		}
	}
	// A Block volume's target is the file Publish created for the bind mount.
	if fi, err := os.Stat(req.GetTargetPath()); err == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
		_ = os.Remove(req.GetTargetPath())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}
