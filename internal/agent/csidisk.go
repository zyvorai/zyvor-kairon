// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/zyvorai/kairon/internal/csinode"
	"github.com/zyvorai/kairon/internal/model"
)

// diskVolumePath is the path FluxVM attaches for a published disk volume:
// disk.img in a Filesystem volume, or the /dev node behind a Block one
// (FluxVM only takes block devices by their /dev path).
func diskVolumePath(v model.DiskVolume) (string, error) {
	if v.Block {
		return realBlockDevice(v.PublishPath)
	}
	return filepath.Join(v.PublishPath, bootDiskFileName), nil
}

// sysDevBlock maps major:minor to block device names; a var for tests.
var sysDevBlock = "/sys/dev/block"

// realBlockDevice returns the /dev path of the block device node at path
// (a CSI publish target is the device bind-mounted over a file).
func realBlockDevice(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return "", fmt.Errorf("%s is not a block device", path)
	}
	rdev := uint64(st.Rdev) //nolint:unconvert // int32 on darwin
	link, err := os.Readlink(filepath.Join(sysDevBlock, fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev))))
	if err != nil {
		return "", fmt.Errorf("find the device behind %s: %w", path, err)
	}
	return filepath.Join("/dev", filepath.Base(link)), nil
}

// diskCSIClient returns the node client for driver: Kairon's own driver or
// one on this node's third-party allowlist.
func (a *Agent) diskCSIClient(driver string) (csi.NodeClient, error) {
	if driver == csinode.DriverName {
		return a.csiNodeClient()
	}
	if _, ok := a.ThirdPartyCSIDrivers[driver]; ok {
		return a.thirdPartyCSIClient(driver)
	}
	return nil, fmt.Errorf("CSI driver %q is neither Kairon's own (%q) nor on this node's third-party allowlist", driver, csinode.DriverName)
}

// publishDiskCSI stages and publishes the CSI-backed pv for spec.disks
// entry disk, under paths keyed by the Machine's runtime name and the disk
// name so one volume per disk can coexist with the boot volume.
func (a *Agent) publishDiskCSI(ctx context.Context, m model.Machine, disk string, pv model.PersistentVolume) (model.DiskVolume, error) {
	src := pv.Spec.CSI
	if a.CSIStagingDir == "" || a.CSIPublishDir == "" {
		return model.DiskVolume{}, fmt.Errorf("this node has no CSI staging/publish directory configured -- see docs/guides/machine-storage-csi.md")
	}
	block := pv.Spec.VolumeMode == "Block"
	own := src.Driver == csinode.DriverName
	client, err := a.diskCSIClient(src.Driver)
	if err != nil {
		return model.DiskVolume{}, err
	}
	var publishContext, stageSecrets, publishSecrets map[string]string
	if own {
		if stageSecrets, err = a.resolveCSIChapSecrets(ctx, src); err != nil {
			return model.DiskVolume{}, err
		}
	} else {
		if publishContext, err = a.ensureCSIAttachment(ctx, pv); err != nil {
			return model.DiskVolume{}, err
		}
		if stageSecrets, err = a.resolveThirdPartyCSISecrets(ctx, "nodeStageSecretRef", src.NodeStageSecretRef); err != nil {
			return model.DiskVolume{}, err
		}
		if publishSecrets, err = a.resolveThirdPartyCSISecrets(ctx, "nodePublishSecretRef", src.NodePublishSecretRef); err != nil {
			return model.DiskVolume{}, err
		}
	}
	v := model.DiskVolume{
		Name:         disk,
		Node:         a.NodeName,
		Driver:       src.Driver,
		VolumeHandle: src.VolumeHandle,
		StagingPath:  filepath.Join(a.CSIStagingDir, "disks", src.Driver, m.RuntimeName(), disk),
		PublishPath:  filepath.Join(a.CSIPublishDir, "disks", src.Driver, m.RuntimeName(), disk),
		Block:        block,
	}
	capability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: src.FSType}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	if block {
		capability.AccessType = &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}
		if err := os.MkdirAll(filepath.Dir(v.PublishPath), 0o750); err != nil {
			return model.DiskVolume{}, fmt.Errorf("create CSI publish parent for PersistentVolume %s: %w", pv.Metadata.Name, err)
		}
	}
	if _, err := client.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          src.VolumeHandle,
		PublishContext:    publishContext,
		StagingTargetPath: v.StagingPath,
		VolumeCapability:  capability,
		VolumeContext:     src.VolumeAttributes,
		Secrets:           stageSecrets,
	}); err != nil {
		return model.DiskVolume{}, fmt.Errorf("NodeStageVolume for PersistentVolume %s (driver %s): %w", pv.Metadata.Name, src.Driver, err)
	}
	if _, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          src.VolumeHandle,
		PublishContext:    publishContext,
		StagingTargetPath: v.StagingPath,
		TargetPath:        v.PublishPath,
		VolumeCapability:  capability,
		VolumeContext:     src.VolumeAttributes,
		Readonly:          src.ReadOnly,
		Secrets:           publishSecrets,
	}); err != nil {
		return model.DiskVolume{}, fmt.Errorf("NodePublishVolume for PersistentVolume %s (driver %s): %w", pv.Metadata.Name, src.Driver, err)
	}
	return v, nil
}

// unpublishDiskCSI reverses publishDiskCSI, releasing the VolumeAttachment
// last for a third-party driver.
func (a *Agent) unpublishDiskCSI(ctx context.Context, v model.DiskVolume) error {
	client, err := a.diskCSIClient(v.Driver)
	if err != nil {
		return err
	}
	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: v.VolumeHandle, TargetPath: v.PublishPath}); err != nil {
		return fmt.Errorf("NodeUnpublishVolume for disk %s (driver %s): %w", v.Name, v.Driver, err)
	}
	if _, err := client.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{VolumeId: v.VolumeHandle, StagingTargetPath: v.StagingPath}); err != nil {
		return fmt.Errorf("NodeUnstageVolume for disk %s (driver %s): %w", v.Name, v.Driver, err)
	}
	if v.Driver == csinode.DriverName {
		return nil
	}
	return a.releaseCSIAttachment(ctx, v.Driver, v.VolumeHandle)
}

// teardownDiskVolumes unpublishes every disk volume this node published
// for m. Entries recorded by another node are left for that node.
func (a *Agent) teardownDiskVolumes(ctx context.Context, m model.Machine) error {
	for _, v := range m.Status.DiskVolumes {
		if v.Node != a.NodeName {
			continue
		}
		if err := a.unpublishDiskCSI(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// storagePathsCondition is the StoragePaths condition for a Machine whose
// boot or data volumes include NVMe-oF volumes of Kairon's own driver on
// this node: True while every portal of every such volume has a live path.
// ok is false when the Machine has none.
func (a *Agent) storagePathsCondition(status model.MachineStatus) (model.Condition, bool) {
	var ids []string
	if status.VolumeHandle != "" && status.VolumeDriver == "" {
		ids = append(ids, status.VolumeHandle)
	}
	for _, v := range status.DiskVolumes {
		if v.Driver == csinode.DriverName && v.Node == a.NodeName {
			ids = append(ids, v.VolumeHandle)
		}
	}
	var msgs []string
	degraded, found := false, false
	for _, id := range ids {
		abnormal, msg, ok := nvmePathCondition(id)
		if !ok {
			continue
		}
		found = true
		degraded = degraded || abnormal
		msgs = append(msgs, msg)
	}
	if !found {
		return model.Condition{}, false
	}
	cond := model.Condition{Type: "StoragePaths", Status: "True", Reason: "AllPathsLive", Message: strings.Join(msgs, " | ")}
	if degraded {
		cond.Status, cond.Reason = "False", "PathsDegraded"
	}
	return cond, true
}

// nvmePathCondition is csinode.NVMePathCondition; a var for tests.
var nvmePathCondition = csinode.NVMePathCondition
