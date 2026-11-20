// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package csinode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// lioClient drives Linux LIO's targetcli CLI (package targetcli-fb on
// Debian/Ubuntu, the kernel's own in-tree SCSI target subsystem
// underneath) to create and destroy exactly the shape of iSCSI
// target/LUN ControllerServer's CreateVolume/DeleteVolume need: one
// fileio backstore, one iSCSI target with one LUN and one portal, per CSI
// volume. Shelling out here is the same deliberate choice
// CommandRunner's own doc comment already makes for iscsiadm/blkid/mkfs
// -- getting raw configfs manipulation exactly right against every
// kernel/LIO version is a solved problem (rtslib, which targetcli itself
// is built on) this project has no interest in re-solving, and every
// command sequence and idempotency error string below was verified
// against a real target_core_mod/targetcli-fb install, not guessed at.
type lioClient struct {
	run CommandRunner
}

// backstoreType is the only LIO backstore type this first cut uses -- a
// plain file on the controller node's own local filesystem, the simplest
// backing store LIO supports and the direct dynamic-provisioning
// equivalent of this driver's existing static "an admin hand-writes a
// PersistentVolume" story, which already assumes nothing more exotic than
// a real block device or file behind the portal it names.
const backstoreType = "fileio"

// ensureBackstore creates a fileio backstore named name, backed by a new
// sparse file at path sized sizeBytes -- targetcli's own fileio create
// auto-creates the backing file at the requested size if it doesn't
// already exist (verified: no separate truncate/fallocate step needed).
// Idempotent: a backstore already existing under this exact name/path
// (a CreateVolume retry with the same idempotency-key-derived name,
// exactly the case the CSI spec requires provisioners to handle safely)
// is treated as success, not an error.
func (c *lioClient) ensureBackstore(ctx context.Context, name, path string, sizeBytes int64) error {
	_, err := c.run.Run(ctx, "targetcli", "/backstores/"+backstoreType, "create",
		"name="+name, "file_or_dev="+path, "size="+strconv.FormatInt(sizeBytes, 10))
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("create %s backstore %s: %w", backstoreType, name, err)
	}
	return nil
}

// deleteBackstore removes the fileio backstore named name -- idempotent,
// tolerant of it already being gone (DeleteVolume must be safe to call
// against an already-deleted volume_id, same requirement NodeUnstageVolume
// already has to meet for its own iSCSI logout).
func (c *lioClient) deleteBackstore(ctx context.Context, name string) error {
	_, err := c.run.Run(ctx, "targetcli", "/backstores/"+backstoreType, "delete", name)
	if err != nil && !strings.Contains(err.Error(), "No storage object named") {
		return fmt.Errorf("delete %s backstore %s: %w", backstoreType, name, err)
	}
	return nil
}

// ensureTarget creates an iSCSI target (and its TPG1 -- LIO always
// creates exactly one Target Portal Group per fresh target, which is all
// this driver ever needs) for iqn. Idempotent, same reasoning as
// ensureBackstore.
func (c *lioClient) ensureTarget(ctx context.Context, iqn string) error {
	_, err := c.run.Run(ctx, "targetcli", "/iscsi", "create", iqn)
	if err != nil && !strings.Contains(err.Error(), "already exists in configFS") {
		return fmt.Errorf("create iscsi target %s: %w", iqn, err)
	}
	return nil
}

// deleteTarget removes iqn's target -- this recursively removes its LUNs,
// portals, and ACLs too, so DeleteVolume never has to tear those down as
// separate steps. Idempotent.
func (c *lioClient) deleteTarget(ctx context.Context, iqn string) error {
	_, err := c.run.Run(ctx, "targetcli", "/iscsi", "delete", iqn)
	if err != nil && !strings.Contains(err.Error(), "No such Target in configfs") {
		return fmt.Errorf("delete iscsi target %s: %w", iqn, err)
	}
	return nil
}

// ensureLUN maps backstoreName's fileio backstore onto iqn's TPG1 as
// LUN 0 -- always LUN 0, matching this driver's existing one-LUN-per-
// target convention (see decodeVolumeID's own default). Idempotent.
func (c *lioClient) ensureLUN(ctx context.Context, iqn, backstoreName string) error {
	_, err := c.run.Run(ctx, "targetcli", tpgPath(iqn)+"/luns", "create", "/backstores/"+backstoreType+"/"+backstoreName)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("create LUN for %s on %s: %w", backstoreName, iqn, err)
	}
	return nil
}

// ensurePortal creates the network portal (address:port) initiators
// dial. LIO's auto_add_default_portal preference already creates a
// 0.0.0.0:3260 portal the instant ensureTarget runs, but that's a global
// preference this driver doesn't control or want to depend on silently
// staying enabled -- so this always issues the create explicitly too,
// tolerant of it already existing either way.
func (c *lioClient) ensurePortal(ctx context.Context, iqn, host, port string) error {
	_, err := c.run.Run(ctx, "targetcli", tpgPath(iqn)+"/portals", "create", host, port)
	if err != nil && !strings.Contains(err.Error(), "already exists in configFS") {
		return fmt.Errorf("create portal %s:%s for %s: %w", host, port, iqn, err)
	}
	return nil
}

// ensureAuth configures iqn's TPG1 to accept any initiator without a
// pre-registered ACL entry (LIO's "demo mode") -- Kairon doesn't track
// which node's initiator IQN will actually consume a dynamically
// provisioned volume ahead of time (CreateVolume runs before any Machine
// is scheduled), so per-initiator ACLs aren't something this first cut
// can populate correctly; demo mode is the same trade network-storage
// systems without a pre-known consumer set commonly make.
//
// username empty (the default, no StorageClass provisioner secret)
// leaves demo mode exactly as before: no CHAP, mirroring this driver's
// documented first-cut posture for the static-PV case (see
// docs/guides/machine-storage-csi.md) -- and the only option a Kairon
// Machine's own boot-disk path can ever use, since kairon-node
// deliberately never resolves a NodeStageSecretRef (same doc, "Real
// limits" -- resolving one would need cluster-wide Secret-read RBAC a
// Machine author could point at anything). username set means a real
// Kubernetes Pod is meant to consume this volume via kubelet, which
// resolves node-stage-secret-name/-namespace with its own already-scoped
// RBAC -- CHAP is only ever a real option on that path. `set attribute`/
// `set auth` are plain assignments, not creates -- always succeed, so
// there is no idempotency error to tolerate here.
func (c *lioClient) ensureAuth(ctx context.Context, iqn, username, password string) error {
	authAttr := "authentication=0"
	if username != "" {
		authAttr = "authentication=1"
	}
	if _, err := c.run.Run(ctx, "targetcli", tpgPath(iqn), "set", "attribute",
		"generate_node_acls=1", "demo_mode_write_protect=0", authAttr); err != nil {
		return fmt.Errorf("configure demo-mode ACLs for %s: %w", iqn, err)
	}
	if username == "" {
		return nil
	}
	if _, err := c.run.Run(ctx, "targetcli", tpgPath(iqn), "set", "auth",
		"userid="+username, "password="+password); err != nil {
		return fmt.Errorf("configure CHAP for %s: %w", iqn, err)
	}
	return nil
}

// resizeBackstore grows name's fileio backstore -- and, per targetcli-fb's
// own documented behavior, its backing sparse file -- to sizeBytes.
// Only ever grows: ControllerExpandVolume's own CSI contract never
// shrinks a volume, and this driver doesn't attempt to guard against a
// caller passing a smaller size (targetcli's own resize command would
// simply do it, the same "trust the CO" posture every other lioClient
// method already takes with its inputs).
func (c *lioClient) resizeBackstore(ctx context.Context, name string, sizeBytes int64) error {
	_, err := c.run.Run(ctx, "targetcli", "/backstores/"+backstoreType+"/"+name, "resize", strconv.FormatInt(sizeBytes, 10))
	if err != nil {
		return fmt.Errorf("resize %s backstore %s to %d bytes: %w", backstoreType, name, sizeBytes, err)
	}
	return nil
}

func tpgPath(iqn string) string {
	return "/iscsi/" + iqn + "/tpg1"
}

// copyFile clones src's contents to dst via `cp --reflink=auto` -- a
// reflink (copy-on-write) clone where the host filesystem supports it
// (btrfs, XFS with reflink=1, overlayfs on either), an instant metadata-
// only operation regardless of file size; a real byte-for-byte copy
// otherwise (coreutils' own automatic, silent fallback -- `--reflink=auto`
// never errors just because the filesystem lacks CoW support, unlike
// `--reflink=always`). Shelling out to `cp` here for the same reason
// lioClient shells out to targetcli: this project has no interest in
// reimplementing FICLONE/copy_file_range ioctl handling and coreutils'
// own fallback behavior in Go. dst must not already exist as a
// directory; an existing regular file at dst is silently overwritten,
// matching plain `cp`'s own default (no -n/--no-clobber).
func (c *lioClient) copyFile(ctx context.Context, src, dst string) error {
	if _, err := c.run.Run(ctx, "cp", "--reflink=auto", src, dst); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}
