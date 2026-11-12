// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Volume context keys a statically-provisioned PersistentVolume's
// spec.csi.volumeAttributes must set for Kairon's iSCSI driver -- see
// docs/guides/machine-storage-csi.md for the full example.
const (
	volumeAttrPortal = "portal" // host:port, e.g. "192.168.1.50:3260"
	volumeAttrIQN    = "iqn"    // target IQN
	volumeAttrLUN    = "lun"    // optional, defaults to "0"

	// SecretKeyUsername/SecretKeyPassword name the two keys an optional
	// nodeStageSecretRef Secret may carry for CHAP authentication --
	// unset means no CHAP, a plain unauthenticated iSCSI login. Exported
	// so kairon-node's own CSI client path can resolve the same keys.
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"

	secretKeyUsername = SecretKeyUsername
	secretKeyPassword = SecretKeyPassword

	defaultLUN    = "0"
	defaultFSType = "ext4"
)

// iscsiConfig is the parsed, validated form of one NodeStageVolumeRequest
// -- the identity of one iSCSI target/LUN this first cut's only backend
// understands. See NodeServer's doc comment for why iSCSI specifically.
type iscsiConfig struct {
	Portal   string
	IQN      string
	LUN      string
	FSType   string
	Username string
	Password string
}

func parseISCSIConfig(volumeContext, secrets map[string]string) (iscsiConfig, error) {
	portal := volumeContext[volumeAttrPortal]
	iqn := volumeContext[volumeAttrIQN]
	if portal == "" || iqn == "" {
		return iscsiConfig{}, fmt.Errorf("volume_context must set %q and %q for Kairon's iSCSI driver", volumeAttrPortal, volumeAttrIQN)
	}
	if err := validateISCSITarget(portal, iqn); err != nil {
		return iscsiConfig{}, err
	}
	lun := volumeContext[volumeAttrLUN]
	if lun == "" {
		lun = defaultLUN
	}
	if _, err := strconv.Atoi(lun); err != nil {
		return iscsiConfig{}, fmt.Errorf("volume_context %q must be an integer LUN number, got %q", volumeAttrLUN, lun)
	}
	return iscsiConfig{
		Portal:   portal,
		IQN:      iqn,
		LUN:      lun,
		FSType:   defaultFSType,
		Username: secrets[secretKeyUsername],
		Password: secrets[secretKeyPassword],
	}, nil
}

// iscsiVolumeIDPrefix marks a CSI volume_id as one of Kairon's own
// encoded iSCSI handles -- see encodeVolumeID.
const iscsiVolumeIDPrefix = "iscsi"

// encodeVolumeID packs enough of cfg into a CSI volume_id string to
// reconstruct it later in NodeUnstageVolume, whose request (per the CSI
// spec) carries only volume_id and staging_target_path -- no
// volume_context, no secrets. This is the value a statically-provisioned
// PersistentVolume's spec.csi.volumeHandle must be set to exactly; see
// docs/guides/machine-storage-csi.md. CHAP credentials are deliberately
// never encoded here (they arrive only via NodeStageVolume's secrets map
// and are never needed again for logout).
func encodeVolumeID(cfg iscsiConfig) string {
	return strings.Join([]string{iscsiVolumeIDPrefix, cfg.Portal, cfg.IQN, cfg.LUN}, "|")
}

func decodeVolumeID(id string) (iscsiConfig, error) {
	parts := strings.Split(id, "|")
	if len(parts) != 4 || parts[0] != iscsiVolumeIDPrefix {
		return iscsiConfig{}, fmt.Errorf("volume_id %q is not a Kairon iSCSI volume handle (expected %q)", id, "iscsi|<portal>|<iqn>|<lun>")
	}
	if err := validateISCSITarget(parts[1], parts[2]); err != nil {
		return iscsiConfig{}, err
	}
	return iscsiConfig{Portal: parts[1], IQN: parts[2], LUN: parts[3]}, nil
}

var (
	// An IQN or portal reaches iscsiadm and targetcli as one argv word and
	// a targetcli path segment: a leading "-" would be read as an option,
	// and "/" or whitespace would escape the path segment.
	iscsiIQNPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:_@-]*$`)
	iscsiPortalPattern = regexp.MustCompile(`^[A-Za-z0-9\[][A-Za-z0-9.:_\[\],-]*$`)
	fsTypePattern      = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
)

func validateISCSITarget(portal, iqn string) error {
	if !iscsiPortalPattern.MatchString(portal) {
		return fmt.Errorf("invalid iSCSI portal %q", portal)
	}
	if !iscsiIQNPattern.MatchString(iqn) {
		return fmt.Errorf("invalid iSCSI target name %q", iqn)
	}
	return nil
}

// deviceWaitTimeout/deviceWaitInterval are vars, not consts, purely so
// tests can shrink them (see setDeviceWaitTimeoutForTest in
// iscsi_test.go) rather than waiting out a real 10s timeout to exercise
// the "device never appeared" error path.
var (
	deviceWaitTimeout  = 10 * time.Second
	deviceWaitInterval = 100 * time.Millisecond
)

// devicePathFunc returns the udev-stable by-path symlink open-iscsi's
// login creates for one target/LUN -- the standard naming convention
// every Linux iSCSI initiator uses, so login doesn't have to parse
// `iscsiadm` output to discover the resulting /dev/sdX. A package-level
// var (not a plain func) so tests can point it at a fixture path instead
// of a real /dev/disk/by-path entry.
var devicePathFunc = func(cfg iscsiConfig) string {
	return fmt.Sprintf("/dev/disk/by-path/ip-%s-iscsi-%s-lun-%s", cfg.Portal, cfg.IQN, cfg.LUN)
}

// iscsiClient drives open-iscsi's iscsiadm CLI. See CommandRunner's own
// doc comment for why this shells out instead of implementing an iSCSI
// initiator in Go.
type iscsiClient struct {
	run CommandRunner
}

// login idempotently logs in to one iSCSI target/LUN and returns the
// real (symlink-resolved) block device path. Safe to call again if
// already logged in: iscsiadm's `--op=new` on an existing node record and
// `--login` on an already-active session both report a recognizable
// "already exists"/"already"-shaped error, which this treats as success
// rather than failure -- required for NodeStageVolume's own idempotency.
func (c *iscsiClient) login(ctx context.Context, cfg iscsiConfig) (string, error) {
	if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "--op=new"); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		return "", fmt.Errorf("create iscsi node record: %w", err)
	}
	if cfg.Username != "" {
		for _, kv := range [][2]string{
			{"node.session.auth.authmethod", "CHAP"},
			{"node.session.auth.username", cfg.Username},
			{"node.session.auth.password", cfg.Password},
		} {
			if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "--op=update", "-n", kv[0], "-v", kv[1]); err != nil {
				return "", fmt.Errorf("configure iscsi CHAP (%s): %w", kv[0], err)
			}
		}
	}
	if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "--login"); err != nil &&
		!strings.Contains(err.Error(), "already") {
		return "", fmt.Errorf("iscsi login: %w", err)
	}

	path := devicePathFunc(cfg)
	deadline := time.Now().Add(deviceWaitTimeout)
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("device %s did not appear within %s of iscsi login -- target/LUN misconfigured, or the node's iscsid isn't running", path, deviceWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(deviceWaitInterval):
		}
	}
}

// logout idempotently logs out of one iSCSI target and removes its node
// record -- safe to call even if already logged out (NodeUnstageVolume
// must be idempotent too).
func (c *iscsiClient) logout(ctx context.Context, cfg iscsiConfig) error {
	if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "--logout"); err != nil &&
		!strings.Contains(err.Error(), "No matching sessions") {
		return fmt.Errorf("iscsi logout: %w", err)
	}
	if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "--op=delete"); err != nil &&
		!strings.Contains(err.Error(), "No records found") {
		return fmt.Errorf("delete iscsi node record: %w", err)
	}
	return nil
}

// rescan asks the initiator to re-read cfg's target/LUN size -- required
// after ControllerExpandVolume has grown the backing LIO backstore and
// before growFilesystem can grow the on-disk filesystem to match: the
// kernel's SCSI layer caches a device's reported size at login and never
// re-reads it on its own.
func (c *iscsiClient) rescan(ctx context.Context, cfg iscsiConfig) error {
	if _, err := c.run.Run(ctx, "iscsiadm", "-m", "node", "-T", cfg.IQN, "-p", cfg.Portal, "-R"); err != nil {
		return fmt.Errorf("rescan iscsi target %s: %w", cfg.IQN, err)
	}
	return nil
}

// growFilesystem grows device's existing filesystem (probed via the same
// blkid TYPE query ensureFormatted already uses) to fill its now-larger
// backing device. ext2/3/4 grow the raw device directly with resize2fs;
// xfs can only grow via an already-mounted mountPath, never the raw
// device -- both are online-grow-only tools, matching this driver's own
// grow-only ControllerExpandVolume contract (the CSI spec has no shrink
// verb either).
func growFilesystem(ctx context.Context, run CommandRunner, device, mountPath string) error {
	fsType, err := run.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		return fmt.Errorf("probe filesystem on %s: %w", device, err)
	}
	fsType = strings.TrimSpace(fsType)
	switch fsType {
	case "ext2", "ext3", "ext4":
		_, err = run.Run(ctx, "resize2fs", device)
	case "xfs":
		_, err = run.Run(ctx, "xfs_growfs", mountPath)
	default:
		return fmt.Errorf("growing a %q filesystem is not supported", fsType)
	}
	if err != nil {
		return fmt.Errorf("grow %s filesystem on %s: %w", fsType, device, err)
	}
	return nil
}

// ensureFormatted formats device with fsType if -- and only if -- blkid
// reports no existing filesystem (exit code 2, blkid's own documented
// signal for "no recognizable filesystem or partition"). Any other
// blkid failure is propagated rather than risking mkfs running over data
// blkid merely failed to identify for some other reason.
func ensureFormatted(ctx context.Context, run CommandRunner, device, fsType string) error {
	if !fsTypePattern.MatchString(fsType) {
		return fmt.Errorf("invalid filesystem type %q", fsType)
	}
	_, err := run.Run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	if err == nil {
		return nil // already formatted
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		if _, mkfsErr := run.Run(ctx, "mkfs."+fsType, device); mkfsErr != nil {
			return fmt.Errorf("format %s as %s: %w", device, fsType, mkfsErr)
		}
		return nil
	}
	return fmt.Errorf("probe filesystem on %s: %w", device, err)
}
