// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//go:build linux

package csinode

import "golang.org/x/sys/unix"

// mount/unmount wrap the real mount(2)/umount(2) syscalls via
// golang.org/x/sys/unix -- pure Go, no `mount`/`umount` binary needed,
// and precise about flags (a bind mount is a real MS_BIND mount, not a
// shelled-out `mount --bind` whose exact argv quoting would otherwise
// need to be gotten right). Linux-only: a CSI node plugin only ever runs
// on a Linux kubelet node, so there is no other platform to support here
// -- see mount_other.go for the stub every other GOOS gets, so the rest
// of this module still builds (but cannot run this path) on a
// non-Linux development machine.
func mount(source, target, fsType string, flags uintptr, data string) error {
	return unix.Mount(source, target, fsType, flags, data)
}

func unmount(target string) error {
	return unix.Unmount(target, 0)
}

const (
	bindMountFlag    = uintptr(unix.MS_BIND)
	bindRemountFlag  = uintptr(unix.MS_REMOUNT)
	bindReadOnlyFlag = uintptr(unix.MS_RDONLY)
)
