// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package csinode

// VolumeStats is a mounted volume's disk usage/capacity, as reported by
// NodeGetVolumeStats -- both bytes and inodes, since a filesystem can run
// out of either independently (see statfs(2)'s f_files/f_ffree). Used is
// computed as Total minus Free (not Total minus Available): Available
// already excludes blocks/inodes the filesystem reserves for its own use
// (e.g. ext4's root-reserved 5%), and CSI's own VolumeUsage.Used is meant
// to answer "how much of this volume's capacity is actually occupied",
// not "how much is left for an unprivileged writer" -- that second
// question is exactly what Available already answers.
type VolumeStats struct {
	TotalBytes, UsedBytes, AvailableBytes    int64
	TotalInodes, UsedInodes, AvailableInodes int64
}
