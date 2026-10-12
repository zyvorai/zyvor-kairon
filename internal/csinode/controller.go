// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// iqnPrefix names every target this driver dynamically provisions --
// deliberately a fixed, stable value (IQN naming convention embeds a
// registration date, which doesn't need to track "today"; it just needs
// to be syntactically valid and never change once volumes exist under
// it). A statically-provisioned PersistentVolume's own hand-written IQN
// (docs/guides/machine-storage-csi.md's example) is unrelated and can be
// anything -- this prefix only marks volumes *this* Controller created,
// so DeleteVolume can recover a dynamic volume's backstore name from its
// IQN (see nameFromIQN).
const iqnPrefix = "iqn.2026-01.dev.zyvor.kairon:"

// defaultVolumeSizeBytes is used only when a CreateVolumeRequest carries
// no CapacityRange at all -- real StorageClass-driven PVCs almost always
// set spec.resources.requests.storage, so this is a defensive fallback,
// not the expected common case. 1Gi matches this driver's own
// docs/guides/machine-storage-csi.md example LUN size.
const defaultVolumeSizeBytes = 1 << 30

// snapshotSubdir is the directory under VolumeDir CreateSnapshot's own
// files live in -- see snapshotFilePath's doc comment.
const snapshotSubdir = ".snapshots"

// volumeNamePattern is what a CSI CreateVolumeRequest.Name is expected to
// look like in practice (Kubernetes' own external-provisioner generates
// "pvc-<uuid>") -- validated defensively since this name becomes both a
// filesystem path component (the backing file) and part of an iSCSI IQN,
// both of which have real character-set constraints this driver would
// rather reject up front than half-succeed against.
var volumeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ControllerServer implements the CSI Controller service for Kairon's
// iSCSI and NVMe-oF backends -- dynamic provisioning, first cut. NVMe-oF
// volumes (StorageClass transport nvme-tcp or nvme-rdma) are served by
// the kernel NVMe target instead of LIO; see createNVMeVolume. Served by the
// separate kairon-csi-controller binary/Deployment (internal/csinode's
// package doc comment explains why it shares this package with
// NodeServer rather than living apart). CreateVolume/DeleteVolume drive a
// real Linux LIO iSCSI target (lioClient) to create/destroy exactly the
// portal/IQN/LUN shape NodeServer's iscsiConfig already knows how to
// consume -- a dynamically provisioned volume is otherwise
// indistinguishable from a statically hand-written one once it reaches
// NodeStageVolume.
//
// Real, current limits (see docs/guides/machine-storage-csi.md): CHAP is
// supported (a provisioner Secret's username/password configure real LIO
// auth instead of demo mode -- see chapCredentialsFromSecrets) and so is
// ControllerExpandVolume -- neither is a gap. CreateSnapshot and
// DeleteSnapshot are implemented too (a copy of the volume's backing file
// under snapshotSubdir). What's still genuinely missing: snapshot export
// off the node, and one backing file per volume on whichever single node runs
// kairon-csi-controller (no topology-aware placement across multiple
// storage nodes -- this first cut assumes one).
type ControllerServer struct {
	csi.UnimplementedControllerServer

	lio *lioClient

	// Portal is the host:port initiators dial -- normally this
	// Controller's own node's real, cluster-reachable IP (or a stable
	// VIP in front of it), never 0.0.0.0. Required; CreateVolume refuses
	// to provision anything without it (nothing else can tell a Machine
	// how to reach a volume this driver creates).
	Portal string

	// VolumeDir is the local directory each backing sparse file is
	// created under -- must exist and be writable, no default (an
	// operator-chosen path, not something this driver invents).
	VolumeDir string

	// NVMePortals are the addresses NVMe-oF volumes (StorageClass
	// parameter transport: nvme-tcp or nvme-rdma) are exported on; every
	// portal of the volume's transport is one path to it. Empty disables
	// NVMe-oF.
	NVMePortals []NVMePortal

	nvmet *nvmetClient
}

// EnableNVMe turns on NVMe-oF provisioning through the kernel target's
// configfs at root (normally /sys/kernel/config/nvmet).
func (s *ControllerServer) EnableNVMe(portals []NVMePortal, root string) error {
	if len(portals) == 0 {
		return fmt.Errorf("at least one NVMe-oF portal is required")
	}
	s.NVMePortals = portals
	s.nvmet = &nvmetClient{Root: root}
	return nil
}

// NewControllerServer builds a ControllerServer against the real
// CommandRunner. portal is required and validated as host:port up front
// (see the Portal field's doc comment) -- a malformed value would
// otherwise only surface on the first real CreateVolume call, the same
// "fail fast at startup, not on the first request" posture this project's
// other TLS/config validation already takes. volumeDir is created (0700
// -- only this Controller process needs it) if it doesn't already exist.
func NewControllerServer(runner CommandRunner, portal, volumeDir string) (*ControllerServer, error) {
	if portal == "" {
		return nil, fmt.Errorf("portal is required")
	}
	if _, _, err := splitPortal(portal); err != nil {
		return nil, err
	}
	if volumeDir == "" {
		return nil, fmt.Errorf("volumeDir is required")
	}
	if err := os.MkdirAll(volumeDir, 0o700); err != nil {
		return nil, fmt.Errorf("create volume directory %s: %w", volumeDir, err)
	}
	return &ControllerServer{lio: &lioClient{run: runner}, Portal: portal, VolumeDir: volumeDir}, nil
}

func (s *ControllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{
				Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			}}},
			{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{
				Type: csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
			}}},
			{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{
				Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
			}}},
		},
	}, nil
}

// chapCredentials is CreateVolume's own read of req.GetSecrets() -- it
// reuses the exact secretKeyUsername/secretKeyPassword keys iscsi.go's
// parseISCSIConfig already expects from a NodeStageVolume secret.
// Pointing a StorageClass's csi.storage.k8s.io/provisioner-secret-name
// and node-stage-secret-name parameters (see docs/guides/machine-storage-csi.md)
// at the same pre-created Secret is what makes the CHAP identity this
// Controller configures on the LIO target match what kubelet later hands
// NodeStageVolume to log in with.
type chapCredentials struct{ Username, Password string }

// chapCredentialsFromSecrets returns a zero-value chapCredentials (demo
// mode, no CHAP) when secrets carries neither key -- the default,
// unchanged behavior for a StorageClass with no provisioner secret.
func chapCredentialsFromSecrets(secrets map[string]string) (chapCredentials, error) {
	c := chapCredentials{Username: secrets[secretKeyUsername], Password: secrets[secretKeyPassword]}
	if (c.Username == "") != (c.Password == "") {
		return chapCredentials{}, fmt.Errorf("a provisioner secret must set both %q and %q, or neither", secretKeyUsername, secretKeyPassword)
	}
	return c, nil
}

// CreateVolume provisions a new iSCSI target/LUN (or, for an NVMe-oF
// transport parameter, an nvmet subsystem) backed by a fresh
// sparse file (or, when VolumeContentSource names a snapshot this
// Controller's own CreateSnapshot produced, a copy of that snapshot's
// content instead), and returns the volume_id/volume_context NodeServer's
// NodeStageVolume already knows how to consume. Idempotent per the CSI
// spec's own requirement for CreateVolume: a retry with the same Name
// (the external-provisioner sidecar's own idempotency token) reaches the
// same already-exists-tolerant lioClient calls and succeeds the same way.
func (s *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if !volumeNamePattern.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "name %q must match %s (Kubernetes' own external-provisioner-generated PVC names always do)", name, volumeNamePattern.String())
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities is required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetMount() == nil && cap.GetBlock() == nil {
			return nil, status.Error(codes.InvalidArgument, "volume capability needs a mount or block access type")
		}
	}
	transport := req.GetParameters()[volumeAttrTransport]
	if transport != "" && transport != transportISCSI {
		t, err := parseNVMeTransport(transport)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return s.createNVMeVolume(ctx, req, name, t)
	}

	sizeBytes := req.GetCapacityRange().GetRequiredBytes()
	if sizeBytes <= 0 {
		sizeBytes = defaultVolumeSizeBytes
	}

	backingPath := s.backingFilePath(name)
	var restoredSize int64
	if src := req.GetVolumeContentSource().GetSnapshot(); src != nil {
		snapshotID := src.GetSnapshotId()
		if !volumeNamePattern.MatchString(snapshotID) {
			return nil, status.Errorf(codes.NotFound, "snapshot %q not found", snapshotID)
		}
		info, err := os.Stat(s.snapshotFilePath(snapshotID))
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "snapshot %q: %v", snapshotID, err)
		}
		restoredSize = info.Size()
		if sizeBytes < restoredSize {
			// The CSI spec requires the new volume's size never be less
			// than its source snapshot's -- silently honor that floor
			// rather than creating a target smaller than the content
			// about to be restored into it.
			sizeBytes = restoredSize
		}
		if _, err := os.Stat(backingPath); os.IsNotExist(err) {
			// Not a CreateVolume retry (the backing file would already
			// exist from a prior attempt) -- actually restore the
			// snapshot's content now, before ensureBackstore below
			// registers this path as a LIO backstore, so the target
			// serves real restored data from its very first read rather
			// than an empty sparse file.
			if err := s.lio.copyFile(ctx, s.snapshotFilePath(snapshotID), backingPath); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
	}

	iqn := iqnPrefix + name
	if err := s.lio.ensureBackstore(ctx, name, backingPath, sizeBytes); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if restoredSize > 0 && sizeBytes > restoredSize {
		// A larger volume than the snapshot itself was requested --
		// ensureBackstore above registered the backstore against the
		// already-restored (smaller) file; grow it the same way
		// ControllerExpandVolume already does for an existing volume.
		if err := s.lio.resizeBackstore(ctx, name, sizeBytes); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if err := s.lio.ensureTarget(ctx, iqn); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.lio.ensureLUN(ctx, iqn, name); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	host, port, err := splitPortal(s.Portal)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.lio.ensurePortal(ctx, iqn, host, port); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	chap, err := chapCredentialsFromSecrets(req.GetSecrets())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.lio.ensureAuth(ctx, iqn, chap.Username, chap.Password); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	cfg := iscsiConfig{Portal: s.Portal, IQN: iqn, LUN: defaultLUN}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      encodeVolumeID(cfg),
		CapacityBytes: sizeBytes,
		ContentSource: req.GetVolumeContentSource(),
		VolumeContext: map[string]string{
			volumeAttrPortal: s.Portal,
			volumeAttrIQN:    iqn,
			volumeAttrLUN:    defaultLUN,
		},
	}}, nil
}

// DeleteVolume tears down the target and backstore CreateVolume created,
// and removes the backing file. Idempotent, including against a
// volume_id this driver never actually recognizes (the CSI spec requires
// DeleteVolume to succeed on an already-gone/unknown volume, not error).
func (s *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if ref, err := decodeAnyVolumeID(req.GetVolumeId()); err == nil && ref.nvme != nil {
		return s.deleteNVMeVolume(*ref.nvme)
	}
	cfg, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return &csi.DeleteVolumeResponse{}, nil
	}
	name, ok := nameFromIQN(cfg.IQN)
	if !ok {
		// A volume_id this driver's own encoding produced, but for a
		// statically-provisioned (not dynamically created) target --
		// nothing this Controller created, so nothing for it to delete.
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err := s.lio.deleteTarget(ctx, cfg.IQN); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.lio.deleteBackstore(ctx, name); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Remove(s.backingFilePath(name)); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "remove backing file for %s: %v", name, err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerExpandVolume grows the LIO backstore CreateVolume created --
// idempotent per the CSI spec: a request whose required_bytes is already
// met by the current backing file's real size succeeds as a no-op rather
// than re-issuing a resize. Always reports NodeExpansionRequired: the
// on-disk filesystem still needs growing separately (NodeServer's own
// NodeExpandVolume) -- the kernel's SCSI layer caches a device's size at
// login and never re-reads it on its own. Never shrinks -- the CSI spec
// itself has no shrink verb.
func (s *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	sizeBytes := req.GetCapacityRange().GetRequiredBytes()
	if sizeBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity_range.required_bytes must be set")
	}
	if ref, err := decodeAnyVolumeID(req.GetVolumeId()); err == nil && ref.nvme != nil {
		return s.expandNVMeVolume(*ref.nvme, sizeBytes)
	}
	cfg, err := decodeVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name, ok := nameFromIQN(cfg.IQN)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "volume %q was not dynamically provisioned by this Controller", req.GetVolumeId())
	}
	info, err := os.Stat(s.backingFilePath(name))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "backing file for %s: %v", name, err)
	}
	if info.Size() >= sizeBytes {
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: info.Size(), NodeExpansionRequired: true}, nil
	}
	if err := s.lio.resizeBackstore(ctx, name, sizeBytes); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: sizeBytes, NodeExpansionRequired: true}, nil
}

func (s *ControllerServer) backingFilePath(name string) string {
	return filepath.Join(s.VolumeDir, name+".img")
}

// snapshotFilePath is CreateSnapshot's own backingFilePath equivalent --
// snapshots live in their own subdirectory under VolumeDir rather than
// alongside live volumes' backing files, purely to make "what's a live
// volume vs. a point-in-time copy" obvious from a directory listing (no
// functional difference otherwise: both are just files this Controller's
// own node owns).
func (s *ControllerServer) snapshotFilePath(name string) string {
	return filepath.Join(s.VolumeDir, snapshotSubdir, name+".img")
}

// CreateSnapshot clones a live volume's backing file into a new,
// independent file under snapshotFilePath -- a real point-in-time copy,
// not a reference into the live volume (so deleting or overwriting the
// source volume afterward never affects a snapshot already taken of it).
// Uses lioClient.copyFile's own reflink-where-possible behavior, so this
// is cheap (a CoW clone, not a full byte copy) on a filesystem that
// supports it. Idempotent per the CSI spec's own requirement: a retry
// with the same Name returns the already-created snapshot rather than
// re-copying or erroring.
func (s *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if !volumeNamePattern.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "name %q must match %s", name, volumeNamePattern.String())
	}
	if req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source_volume_id is required")
	}
	dst := s.snapshotFilePath(name)
	if info, err := os.Stat(dst); err == nil {
		return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
			SnapshotId: name, SourceVolumeId: req.GetSourceVolumeId(),
			SizeBytes: info.Size(), CreationTime: timestamppb.New(info.ModTime()), ReadyToUse: true,
		}}, nil
	}
	volName, ok, err := volumeNameFromID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "source volume %q: %v", req.GetSourceVolumeId(), err)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "source volume %q was not dynamically provisioned by this Controller", req.GetSourceVolumeId())
	}
	srcInfo, err := os.Stat(s.backingFilePath(volName))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "source volume %q backing file: %v", req.GetSourceVolumeId(), err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.lio.copyFile(ctx, s.backingFilePath(volName), dst); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SnapshotId: name, SourceVolumeId: req.GetSourceVolumeId(),
		SizeBytes: srcInfo.Size(), CreationTime: timestamppb.Now(), ReadyToUse: true,
	}}, nil
}

// DeleteSnapshot removes the file CreateSnapshot created. Idempotent,
// including against a snapshot_id this driver never actually recognizes
// (the CSI spec requires DeleteSnapshot to succeed on an already-gone/
// unknown snapshot, not error) -- the same posture DeleteVolume already
// takes for an unrecognized volume_id.
func (s *ControllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_id is required")
	}
	if !volumeNamePattern.MatchString(req.GetSnapshotId()) {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	if err := os.Remove(s.snapshotFilePath(req.GetSnapshotId())); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "remove snapshot file for %s: %v", req.GetSnapshotId(), err)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// nameFromIQN recovers CreateVolume's original name from an IQN this
// driver generated (iqnPrefix + name) -- ok is false for any IQN not
// carrying this exact prefix, which DeleteVolume treats as "nothing this
// Controller provisioned."
func nameFromIQN(iqn string) (name string, ok bool) {
	if !strings.HasPrefix(iqn, iqnPrefix) {
		return "", false
	}
	return strings.TrimPrefix(iqn, iqnPrefix), true
}

func splitPortal(portal string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(portal)
	if err != nil {
		return "", "", fmt.Errorf("portal %q must be host:port: %w", portal, err)
	}
	return host, port, nil
}

// volumeNameFromID is the CreateVolume name behind a volume_id of either
// backend; ok is false for a statically provisioned volume.
func volumeNameFromID(id string) (name string, ok bool, err error) {
	ref, err := decodeAnyVolumeID(id)
	if err != nil {
		return "", false, err
	}
	if ref.nvme != nil {
		name, ok = nameFromNQN(ref.nvme.NQN)
		return name, ok, nil
	}
	name, ok = nameFromIQN(ref.iscsi.IQN)
	return name, ok, nil
}

// prepareBackingFile creates name's backing file at sizeBytes, from the
// snapshot req names when it names one. Returns the final size.
func (s *ControllerServer) prepareBackingFile(ctx context.Context, req *csi.CreateVolumeRequest, name string, sizeBytes int64) (int64, error) {
	backingPath := s.backingFilePath(name)
	if src := req.GetVolumeContentSource().GetSnapshot(); src != nil {
		snapshotID := src.GetSnapshotId()
		if !volumeNamePattern.MatchString(snapshotID) {
			return 0, status.Errorf(codes.NotFound, "snapshot %q not found", snapshotID)
		}
		info, err := os.Stat(s.snapshotFilePath(snapshotID))
		if err != nil {
			return 0, status.Errorf(codes.NotFound, "snapshot %q: %v", snapshotID, err)
		}
		if sizeBytes < info.Size() {
			sizeBytes = info.Size()
		}
		if _, err := os.Stat(backingPath); os.IsNotExist(err) {
			if err := s.lio.copyFile(ctx, s.snapshotFilePath(snapshotID), backingPath); err != nil {
				return 0, status.Error(codes.Internal, err.Error())
			}
		}
	}
	f, err := os.OpenFile(backingPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "create backing file for %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, status.Error(codes.Internal, err.Error())
	}
	if info.Size() < sizeBytes {
		if err := f.Truncate(sizeBytes); err != nil {
			return 0, status.Errorf(codes.Internal, "size backing file for %s: %v", name, err)
		}
	} else {
		sizeBytes = info.Size()
	}
	return sizeBytes, nil
}

// createNVMeVolume serves a sparse backing file as namespace 1 of a new
// nvmet subsystem, exported on every configured portal of transport t.
func (s *ControllerServer) createNVMeVolume(ctx context.Context, req *csi.CreateVolumeRequest, name string, t nvmeTransport) (*csi.CreateVolumeResponse, error) {
	if s.nvmet == nil {
		return nil, status.Errorf(codes.InvalidArgument, "transport %s: this controller has no NVMe-oF portals (-nvme-portals)", t)
	}
	// A portal's nvmet port id is its position in NVMePortals, so it stays
	// the same for every volume.
	var portals []string
	ports := map[int]NVMePortal{}
	for i, p := range s.NVMePortals {
		if p.Transport == t {
			portals = append(portals, p.addr())
			ports[nvmetPortIDBase+i] = p
		}
	}
	if len(portals) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "transport %s: no %s portal is configured (-nvme-portals)", t, t.kernel())
	}
	if req.GetSecrets()[secretKeyUsername] != "" {
		return nil, status.Error(codes.InvalidArgument, "NVMe-oF volumes do not support CHAP secrets (no DH-CHAP yet)")
	}
	sizeBytes := req.GetCapacityRange().GetRequiredBytes()
	if sizeBytes <= 0 {
		sizeBytes = defaultVolumeSizeBytes
	}
	sizeBytes, err := s.prepareBackingFile(ctx, req, name, sizeBytes)
	if err != nil {
		return nil, err
	}
	nqn := nqnPrefix + name
	if err := s.nvmet.ensureSubsystem(nqn, s.backingFilePath(name)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for id, p := range ports {
		if err := s.nvmet.ensurePort(id, p); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if err := s.nvmet.linkPort(id, nqn); err != nil {
			return nil, status.Errorf(codes.Internal, "portal %s: %v", p.addr(), err)
		}
	}
	cfg := nvmeConfig{Transport: t, NQN: nqn, Portals: portals}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      encodeNVMeVolumeID(cfg),
		CapacityBytes: sizeBytes,
		ContentSource: req.GetVolumeContentSource(),
		VolumeContext: map[string]string{
			volumeAttrTransport: string(t),
			volumeAttrNQN:       nqn,
			volumeAttrPortals:   strings.Join(portals, ","),
		},
	}}, nil
}

func (s *ControllerServer) deleteNVMeVolume(cfg nvmeConfig) (*csi.DeleteVolumeResponse, error) {
	name, ok := nameFromNQN(cfg.NQN)
	if !ok || s.nvmet == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err := s.nvmet.deleteSubsystem(cfg.NQN); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Remove(s.backingFilePath(name)); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "remove backing file for %s: %v", name, err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// expandNVMeVolume grows the backing file and has nvmet re-read its size;
// connected hosts get a namespace-changed notice.
func (s *ControllerServer) expandNVMeVolume(cfg nvmeConfig, sizeBytes int64) (*csi.ControllerExpandVolumeResponse, error) {
	name, ok := nameFromNQN(cfg.NQN)
	if !ok || s.nvmet == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s was not dynamically provisioned by this Controller", cfg.NQN)
	}
	path := s.backingFilePath(name)
	info, err := os.Stat(path)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "backing file for %s: %v", name, err)
	}
	if info.Size() >= sizeBytes {
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: info.Size(), NodeExpansionRequired: true}, nil
	}
	if err := os.Truncate(path, sizeBytes); err != nil {
		return nil, status.Errorf(codes.Internal, "grow backing file for %s: %v", name, err)
	}
	if err := s.nvmet.revalidateSize(cfg.NQN); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: sizeBytes, NodeExpansionRequired: true}, nil
}
