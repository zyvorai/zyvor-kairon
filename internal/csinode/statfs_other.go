// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

//go:build !linux

package csinode

import (
	"fmt"
	"runtime"
)

// statfs on any non-Linux GOOS always fails -- a CSI node plugin only
// ever runs on a Linux kubelet node in production (see statfs_linux.go
// for the real implementation); this stub exists purely so
// `go build`/`go vet`/`golangci-lint` succeed on a non-Linux development
// machine (this project is developed on macOS), not because this path is
// ever meant to run for real. Matches mount_other.go's own posture.
func statfs(path string) (VolumeStats, error) {
	return VolumeStats{}, fmt.Errorf("csinode: statfs is only supported on linux (GOOS=%s)", runtime.GOOS)
}
