// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//go:build linux

package csinode

import "syscall"

// statfs reads path's filesystem-level usage/capacity via the statfs(2)
// syscall from the standard library -- pure Go, no `df` binary needed,
// same posture as IsMountPoint parsing /proc/self/mountinfo directly
// instead of shelling out to `findmnt`. Linux-only, matching
// mount_linux.go: a CSI node plugin only ever runs on a Linux kubelet
// node, so there is no other platform to support here for real -- see
// statfs_other.go for the stub every other GOOS gets so the rest of this
// module still builds (but cannot run this path) on a non-Linux
// development machine.
func statfs(path string) (VolumeStats, error) {
	var buf syscall.Statfs_t
	if err := syscall.Statfs(path, &buf); err != nil {
		return VolumeStats{}, err
	}
	bsize := buf.Bsize
	total := int64(buf.Blocks) * bsize
	free := int64(buf.Bfree) * bsize
	avail := int64(buf.Bavail) * bsize
	totalInodes := int64(buf.Files)
	freeInodes := int64(buf.Ffree)
	return VolumeStats{
		TotalBytes:      total,
		UsedBytes:       total - free,
		AvailableBytes:  avail,
		TotalInodes:     totalInodes,
		UsedInodes:      totalInodes - freeInodes,
		AvailableInodes: freeInodes,
	}, nil
}
