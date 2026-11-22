// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package csinode

import (
	"context"
	"errors"
	"testing"
)

// Every idempotency error string below (and every exact command argv this
// file asserts against) was verified against a real target_core_mod/
// targetcli-fb install on a real host -- see lioClient's own doc comment.

func TestEnsureBackstoreCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}

	if err := c.ensureBackstore(context.Background(), "vol1", "/var/lib/kairon/csi-volumes/vol1.img", 1073741824); err != nil {
		t.Fatalf("ensureBackstore: %v", err)
	}
	calls := run.callsFor("targetcli")
	if len(calls) != 1 {
		t.Fatalf("expected 1 targetcli call, got %d", len(calls))
	}
	got := joinArgs(calls[0][1:])
	want := "/backstores/fileio create name=vol1 file_or_dev=/var/lib/kairon/csi-volumes/vol1.img size=1073741824"
	if got != want {
		t.Fatalf("unexpected command:\n got: %s\nwant: %s", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) {
		return "", errors.New("storage object for /path already exists: vol1")
	})
	if err := c.ensureBackstore(context.Background(), "vol1", "/path", 1073741824); err != nil {
		t.Fatalf("expected already-exists to be tolerated, got %v", err)
	}
}

func TestEnsureBackstorePropagatesOtherErrors(t *testing.T) {
	run := newFakeCommandRunner()
	run.on("targetcli", func(args ...string) (string, error) { return "", errors.New("permission denied") })
	c := &lioClient{run: run}
	if err := c.ensureBackstore(context.Background(), "vol1", "/path", 1); err == nil {
		t.Fatal("expected a non-tolerated error to propagate")
	}
}

func TestDeleteBackstoreCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}

	if err := c.deleteBackstore(context.Background(), "vol1"); err != nil {
		t.Fatalf("deleteBackstore: %v", err)
	}
	calls := run.callsFor("targetcli")
	if got, want := joinArgs(calls[0][1:]), "/backstores/fileio delete vol1"; got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) { return "", errors.New("No storage object named vol1.") })
	if err := c.deleteBackstore(context.Background(), "vol1"); err != nil {
		t.Fatalf("expected not-found to be tolerated, got %v", err)
	}
}

func TestEnsureTargetCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.ensureTarget(context.Background(), iqn); err != nil {
		t.Fatalf("ensureTarget: %v", err)
	}
	calls := run.callsFor("targetcli")
	if got, want := joinArgs(calls[0][1:]), "/iscsi create "+iqn; got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) { return "", errors.New("This Target already exists in configFS") })
	if err := c.ensureTarget(context.Background(), iqn); err != nil {
		t.Fatalf("expected already-exists to be tolerated, got %v", err)
	}
}

func TestDeleteTargetCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.deleteTarget(context.Background(), iqn); err != nil {
		t.Fatalf("deleteTarget: %v", err)
	}
	calls := run.callsFor("targetcli")
	if got, want := joinArgs(calls[0][1:]), "/iscsi delete "+iqn; got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) {
		return "", errors.New("No such Target in configfs: /sys/kernel/config/target/iscsi/" + iqn)
	})
	if err := c.deleteTarget(context.Background(), iqn); err != nil {
		t.Fatalf("expected not-found to be tolerated, got %v", err)
	}
}

func TestEnsureLUNCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.ensureLUN(context.Background(), iqn, "vol1"); err != nil {
		t.Fatalf("ensureLUN: %v", err)
	}
	calls := run.callsFor("targetcli")
	want := "/iscsi/" + iqn + "/tpg1/luns create /backstores/fileio/vol1"
	if got := joinArgs(calls[0][1:]); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) {
		return "", errors.New("lun for storage object fileio/vol1 already exists")
	})
	if err := c.ensureLUN(context.Background(), iqn, "vol1"); err != nil {
		t.Fatalf("expected already-exists to be tolerated, got %v", err)
	}
}

func TestEnsurePortalCommandShapeAndTolerance(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.ensurePortal(context.Background(), iqn, "10.0.0.5", "3260"); err != nil {
		t.Fatalf("ensurePortal: %v", err)
	}
	calls := run.callsFor("targetcli")
	want := "/iscsi/" + iqn + "/tpg1/portals create 10.0.0.5 3260"
	if got := joinArgs(calls[0][1:]); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) {
		return "", errors.New("This NetworkPortal already exists in configFS")
	})
	if err := c.ensurePortal(context.Background(), iqn, "10.0.0.5", "3260"); err != nil {
		t.Fatalf("expected already-exists to be tolerated, got %v", err)
	}
}

func TestEnsureAuthDemoModeCommandShape(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.ensureAuth(context.Background(), iqn, "", ""); err != nil {
		t.Fatalf("ensureAuth: %v", err)
	}
	calls := run.callsFor("targetcli")
	if len(calls) != 1 {
		t.Fatalf("expected 1 targetcli call for demo mode (no CHAP), got %d", len(calls))
	}
	want := "/iscsi/" + iqn + "/tpg1 set attribute generate_node_acls=1 demo_mode_write_protect=0 authentication=0"
	if got := joinArgs(calls[0][1:]); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) { return "", errors.New("boom") })
	if err := c.ensureAuth(context.Background(), iqn, "", ""); err == nil {
		t.Fatal("expected a real failure to propagate (set attribute has no idempotency error to tolerate)")
	}
}

func TestEnsureAuthCHAPCommandShape(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}
	iqn := "iqn.2026-01.dev.zyvor.kairon:vol1"

	if err := c.ensureAuth(context.Background(), iqn, "alice", "s3cret"); err != nil {
		t.Fatalf("ensureAuth: %v", err)
	}
	calls := run.callsFor("targetcli")
	if len(calls) != 2 {
		t.Fatalf("expected 2 targetcli calls (demo-mode attrs + CHAP creds), got %d", len(calls))
	}
	wantAttr := "/iscsi/" + iqn + "/tpg1 set attribute generate_node_acls=1 demo_mode_write_protect=0 authentication=1"
	if got := joinArgs(calls[0][1:]); got != wantAttr {
		t.Fatalf("unexpected attribute command: got %q want %q", got, wantAttr)
	}
	wantAuth := "/iscsi/" + iqn + "/tpg1 set auth userid=alice password=s3cret"
	if got := joinArgs(calls[1][1:]); got != wantAuth {
		t.Fatalf("unexpected auth command: got %q want %q", got, wantAuth)
	}
}

func TestResizeBackstoreCommandShape(t *testing.T) {
	run := newFakeCommandRunner()
	c := &lioClient{run: run}

	if err := c.resizeBackstore(context.Background(), "vol1", 2147483648); err != nil {
		t.Fatalf("resizeBackstore: %v", err)
	}
	calls := run.callsFor("targetcli")
	want := "/backstores/fileio/vol1 resize 2147483648"
	if got := joinArgs(calls[0][1:]); got != want {
		t.Fatalf("unexpected command: got %q want %q", got, want)
	}

	run.on("targetcli", func(args ...string) (string, error) { return "", errors.New("boom") })
	if err := c.resizeBackstore(context.Background(), "vol1", 2147483648); err == nil {
		t.Fatal("expected a real failure to propagate")
	}
}
