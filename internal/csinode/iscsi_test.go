// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package csinode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseISCSIConfigRequiresPortalAndIQN(t *testing.T) {
	if _, err := parseISCSIConfig(nil, nil); err == nil {
		t.Fatal("expected an error for an empty volume_context")
	}
	if _, err := parseISCSIConfig(map[string]string{volumeAttrPortal: "10.0.0.1:3260"}, nil); err == nil {
		t.Fatal("expected an error when iqn is missing")
	}
}

func TestParseISCSIConfigDefaultsLUNAndFSType(t *testing.T) {
	cfg, err := parseISCSIConfig(map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"}, nil)
	if err != nil {
		t.Fatalf("parseISCSIConfig: %v", err)
	}
	if cfg.LUN != defaultLUN {
		t.Fatalf("expected default LUN %q, got %q", defaultLUN, cfg.LUN)
	}
	if cfg.FSType != defaultFSType {
		t.Fatalf("expected default fsType %q, got %q", defaultFSType, cfg.FSType)
	}
}

func TestParseISCSIConfigRejectsNonIntegerLUN(t *testing.T) {
	_, err := parseISCSIConfig(map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk", volumeAttrLUN: "not-a-number"}, nil)
	if err == nil {
		t.Fatal("expected an error for a non-integer lun")
	}
}

func TestParseISCSIConfigReadsCHAPSecrets(t *testing.T) {
	cfg, err := parseISCSIConfig(
		map[string]string{volumeAttrPortal: "10.0.0.1:3260", volumeAttrIQN: "iqn.test:disk"},
		map[string]string{secretKeyUsername: "alice", secretKeyPassword: "s3cret"},
	)
	if err != nil {
		t.Fatalf("parseISCSIConfig: %v", err)
	}
	if cfg.Username != "alice" || cfg.Password != "s3cret" {
		t.Fatalf("expected CHAP creds to be read from secrets, got %+v", cfg)
	}
}

func TestVolumeIDRoundTrips(t *testing.T) {
	cfg := iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "3"}
	id := encodeVolumeID(cfg)
	got, err := decodeVolumeID(id)
	if err != nil {
		t.Fatalf("decodeVolumeID: %v", err)
	}
	if got.Portal != cfg.Portal || got.IQN != cfg.IQN || got.LUN != cfg.LUN {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestDecodeVolumeIDRejectsUnrecognizedFormat(t *testing.T) {
	for _, id := range []string{"", "not-kairon-shaped", "rbd|pool|image"} {
		if _, err := decodeVolumeID(id); err == nil {
			t.Fatalf("expected an error decoding %q", id)
		}
	}
}

func TestISCSILoginRunsExpectedCommandSequence(t *testing.T) {
	run := newFakeCommandRunner()
	cfg := iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}

	// Stand in for the by-path symlink iscsiadm's real login creates --
	// devicePath resolves to a location under t.TempDir() instead of the
	// real /dev/disk/by-path, so filepath.EvalSymlinks has something to
	// actually find.
	dir := t.TempDir()
	realDevice := dir + "/sda"
	if err := writeFile(realDevice, "not a real block device, just needs to exist"); err != nil {
		t.Fatalf("write fake device: %v", err)
	}
	symlink := dir + "/by-path-link"
	if err := symlinkFile(realDevice, symlink); err != nil {
		t.Fatalf("symlink fake device: %v", err)
	}
	restoreDevicePath := setDevicePathForTest(func(iscsiConfig) string { return symlink })
	defer restoreDevicePath()

	c := &iscsiClient{run: run}
	device, err := c.login(context.Background(), cfg)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	// EvalSymlinks canonicalizes the whole path, not just the final
	// component (e.g. macOS's /var -> /private/var) -- resolve
	// realDevice the same way before comparing, rather than asserting an
	// exact string match against a path the OS itself may rewrite.
	wantDevice, err := filepath.EvalSymlinks(realDevice)
	if err != nil {
		t.Fatalf("resolve expected device: %v", err)
	}
	if device != wantDevice {
		t.Fatalf("expected resolved device %q, got %q", wantDevice, device)
	}

	if len(run.callsFor("iscsiadm")) != 2 {
		t.Fatalf("expected exactly 2 iscsiadm calls (op=new, login) with no CHAP configured, got %v", run.calls)
	}
	loginCalls := run.callsFor("iscsiadm")
	if !strings.Contains(joinArgs(loginCalls[1]), "--login") {
		t.Fatalf("expected the second iscsiadm call to log in, got %v", loginCalls[1])
	}
}

func TestISCSILoginConfiguresCHAPWhenCredentialsSet(t *testing.T) {
	run := newFakeCommandRunner()
	cfg := iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0", Username: "alice", Password: "s3cret"}

	dir := t.TempDir()
	realDevice := dir + "/sda"
	_ = writeFile(realDevice, "x")
	symlink := dir + "/by-path-link"
	_ = symlinkFile(realDevice, symlink)
	restore := setDevicePathForTest(func(iscsiConfig) string { return symlink })
	defer restore()

	c := &iscsiClient{run: run}
	if _, err := c.login(context.Background(), cfg); err != nil {
		t.Fatalf("login: %v", err)
	}
	calls := run.callsFor("iscsiadm")
	var chapUsernameSet bool
	for _, call := range calls {
		joined := joinArgs(call)
		if strings.Contains(joined, "node.session.auth.username") && strings.Contains(joined, "alice") {
			chapUsernameSet = true
		}
	}
	if !chapUsernameSet {
		t.Fatalf("expected a CHAP username update call, got %v", calls)
	}
}

func TestISCSILoginTreatsAlreadyActiveSessionAsSuccess(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("iscsiadm", func(args ...string) (string, error) {
		if len(args) > 0 && args[len(args)-1] == "--login" {
			return "", errors.New("iscsiadm: default: 1 session requested, but 1 already present.\nalready logged in")
		}
		return "", nil
	})
	cfg := iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}
	dir := t.TempDir()
	realDevice := dir + "/sda"
	_ = writeFile(realDevice, "x")
	symlink := dir + "/by-path-link"
	_ = symlinkFile(realDevice, symlink)
	restore := setDevicePathForTest(func(iscsiConfig) string { return symlink })
	defer restore()

	c := &iscsiClient{run: run}
	if _, err := c.login(context.Background(), cfg); err != nil {
		t.Fatalf("expected an 'already logged in' response to be treated as success, got: %v", err)
	}
}

func TestISCSILoginFailsIfDeviceNeverAppears(t *testing.T) {
	run := newFakeCommandRunner()
	cfg := iscsiConfig{Portal: "10.0.0.1:3260", IQN: "iqn.test:disk", LUN: "0"}
	restore := setDevicePathForTest(func(iscsiConfig) string { return "/nonexistent/path/that/will/never/appear" })
	defer restore()
	restoreTimeout := setDeviceWaitTimeoutForTest()
	defer restoreTimeout()

	c := &iscsiClient{run: run}
	if _, err := c.login(context.Background(), cfg); err == nil {
		t.Fatal("expected an error when the device never appears")
	}
}

func TestISCSILogoutIgnoresNoMatchingSessions(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("iscsiadm", func(args ...string) (string, error) {
		if len(args) > 0 && args[len(args)-1] == "--logout" {
			return "", errors.New("iscsiadm: No matching sessions found")
		}
		return "", nil // --op=delete succeeds normally
	})
	c := &iscsiClient{run: run}
	if err := c.logout(context.Background(), iscsiConfig{Portal: "p", IQN: "i", LUN: "0"}); err != nil {
		t.Fatalf("expected 'no matching sessions' to be treated as success, got: %v", err)
	}
}

func TestISCSILogoutIgnoresNoRecordsFoundOnDelete(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("iscsiadm", func(args ...string) (string, error) {
		if len(args) > 0 && args[len(args)-1] == "--op=delete" {
			return "", errors.New("iscsiadm: No records found")
		}
		return "", nil
	})
	c := &iscsiClient{run: run}
	if err := c.logout(context.Background(), iscsiConfig{Portal: "p", IQN: "i", LUN: "0"}); err != nil {
		t.Fatalf("expected 'no records found' on delete to be treated as success, got: %v", err)
	}
}

func TestEnsureFormattedSkipsAlreadyFormattedDevice(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "ext4", nil })
	run.on("mkfs.ext4", func(args ...string) (string, error) {
		t.Fatal("mkfs should not run when blkid reports an existing filesystem")
		return "", nil
	})
	if err := ensureFormatted(context.Background(), run, "/dev/fake", "ext4"); err != nil {
		t.Fatalf("ensureFormatted: %v", err)
	}
}

func TestEnsureFormattedFormatsWhenBlkidReportsNoFilesystem(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "", fakeExitError(2) })
	if err := ensureFormatted(context.Background(), run, "/dev/fake", "ext4"); err != nil {
		t.Fatalf("ensureFormatted: %v", err)
	}
	if len(run.callsFor("mkfs.ext4")) != 1 {
		t.Fatalf("expected exactly one mkfs.ext4 call, got %v", run.calls)
	}
}

func TestEnsureFormattedPropagatesUnexpectedBlkidFailure(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "", fakeExitError(4) }) // ambiguous probe result, not "no filesystem"
	if err := ensureFormatted(context.Background(), run, "/dev/fake", "ext4"); err == nil {
		t.Fatal("expected a non-exit-2 blkid failure to be propagated, not treated as unformatted")
	}
	if len(run.callsFor("mkfs.ext4")) != 0 {
		t.Fatal("expected mkfs not to run when blkid's failure isn't the documented 'no filesystem' signal")
	}
}

func TestRescanCommandShape(t *testing.T) {
	run := newFakeCommandRunner()
	c := &iscsiClient{run: run}
	if err := c.rescan(context.Background(), iscsiConfig{Portal: "10.0.0.5:3260", IQN: "iqn.x", LUN: "0"}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	calls := run.callsFor("iscsiadm")
	want := "-m node -T iqn.x -p 10.0.0.5:3260 -R"
	if got := joinArgs(calls[0][1:]); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}
}

func TestRescanPropagatesFailure(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("iscsiadm", func(args ...string) (string, error) { return "", errors.New("boom") })
	c := &iscsiClient{run: run}
	if err := c.rescan(context.Background(), iscsiConfig{Portal: "p", IQN: "i", LUN: "0"}); err == nil {
		t.Fatal("expected a failure to propagate")
	}
}

func TestGrowFilesystemExt4UsesResize2fs(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "ext4", nil })
	if err := growFilesystem(context.Background(), run, "/dev/sda", "/mnt/x"); err != nil {
		t.Fatalf("growFilesystem: %v", err)
	}
	calls := run.callsFor("resize2fs")
	if len(calls) != 1 || joinArgs(calls[0][1:]) != "/dev/sda" {
		t.Fatalf("expected resize2fs against the device, got %+v", calls)
	}
}

func TestGrowFilesystemXFSUsesXFSGrowfsAgainstMountPath(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "xfs", nil })
	if err := growFilesystem(context.Background(), run, "/dev/sda", "/mnt/x"); err != nil {
		t.Fatalf("growFilesystem: %v", err)
	}
	calls := run.callsFor("xfs_growfs")
	if len(calls) != 1 || joinArgs(calls[0][1:]) != "/mnt/x" {
		t.Fatalf("expected xfs_growfs against the mount path, got %+v", calls)
	}
}

func TestGrowFilesystemRejectsUnsupportedType(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "btrfs", nil })
	if err := growFilesystem(context.Background(), run, "/dev/sda", "/mnt/x"); err == nil {
		t.Fatal("expected an error for an unsupported filesystem type")
	}
}

func TestGrowFilesystemPropagatesBlkidFailure(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("blkid", func(args ...string) (string, error) { return "", errors.New("boom") })
	if err := growFilesystem(context.Background(), run, "/dev/sda", "/mnt/x"); err == nil {
		t.Fatal("expected a blkid failure to propagate")
	}
}
