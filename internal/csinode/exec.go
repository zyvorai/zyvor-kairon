// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package csinode implements the CSI (Container Storage Interface) Node
// and Controller services for Kairon's first-cut network-block volume
// driver: one backend (iSCSI, real Linux LIO targets), and the real,
// current limits documented in docs/guides/machine-storage-csi.md. The
// Node service (NodeServer, served by cmd/kairon-csi-node) and the
// Controller service (ControllerServer, served by
// cmd/kairon-csi-controller) live in the same package -- they share the
// exact same iSCSI domain model (iscsiConfig, encodeVolumeID/
// decodeVolumeID) since a volume the Controller dynamically provisions is
// consumed by the Node service exactly like a statically-provisioned one
// always was, just with its portal/IQN/LUN generated instead of
// hand-written into a PersistentVolume. They're still served from two
// different binaries/processes/gRPC endpoints (a Node DaemonSet pod and a
// Controller Deployment pod), matching standard CSI deployment topology.
//
// This package -- along with cmd/kairon-csi-node and
// cmd/kairon-csi-controller, which serve it over gRPC -- is the second
// deliberate exception to Kairon's Go-stdlib-only design guarantee (the
// first is kairon-ui's optional OIDC/SSO): the CSI protocol itself is a
// gRPC/protobuf wire contract kubelet speaks to a Unix socket, so there is
// no stdlib-only way to implement it at all. kairon-controller/
// kairon-node's own core VM orchestration pulls in none of this.
package csinode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandRunner runs an external command and returns its combined
// stdout+stderr, trimmed of trailing whitespace. A small seam so the
// iSCSI login/logout/format orchestration in this package (retry logic,
// idempotency checks, error propagation) is unit-testable against a fake
// -- see iscsi_test.go -- without a real iscsiadm/blkid/mkfs binary, a
// real iSCSI target, or root privileges, none of which are available in
// this project's own CI or dev sandboxes. The actual OS-level behavior of
// the real commands this shells out to can only be verified by running
// the built kairon-csi-node image against a real target -- see
// docs/guides/machine-storage-csi.md's own note on this.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (output string, err error)
}

// execRunner is the real CommandRunner. Shelling out is deliberate here,
// not a shortcut: a full iSCSI initiator (open-iscsi's iscsiadm),
// filesystem probing (blkid), and filesystem creation (mkfs.*, from
// e2fsprogs/xfsprogs) are not things this project reimplements in Go --
// each is its own mature, widely-deployed tool this driver's container
// image installs alongside the kairon-csi-node binary (see the
// kairon-csi-node Dockerfile stage, which is why it isn't built on the
// same minimal distroless base as kairon-controller/kairon-node).
type execRunner struct{}

// NewCommandRunner returns the real, os/exec-backed CommandRunner.
func NewCommandRunner() CommandRunner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return trimmed, fmt.Errorf("%s %s: %w: %s", name, strings.Join(redactArgs(args), " "), err, trimmed)
	}
	return trimmed, nil
}

// redactArgs masks CHAP secrets in a command line before it lands in an
// error: the error is returned to kubelet over gRPC and shows up in Pod
// events, so `-v <password>` and `password=<secret>` must not survive it.
func redactArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		switch {
		case strings.HasPrefix(a, "password="):
			out[i] = "password=***"
		case a == "-v" && i >= 2 && out[i-2] == "-n" && strings.HasSuffix(out[i-1], ".password"):
			if i+1 < len(out) {
				out[i+1] = "***"
			}
		}
	}
	return out
}
