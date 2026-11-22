// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

//go:build !linux

package csinode

import (
	"fmt"
	"runtime"
)

// mount/unmount on any non-Linux GOOS always fail -- a CSI node plugin
// only ever runs on a Linux kubelet node in production (see
// mount_linux.go for the real implementation); this stub exists purely
// so `go build`/`go vet`/`golangci-lint` succeed on a non-Linux
// development machine (this project is developed on macOS), not because
// this path is ever meant to run for real.
func mount(source, target, fsType string, flags uintptr, data string) error {
	return fmt.Errorf("csinode: mount is only supported on linux (GOOS=%s)", runtime.GOOS)
}

func unmount(target string) error {
	return fmt.Errorf("csinode: unmount is only supported on linux (GOOS=%s)", runtime.GOOS)
}

const (
	bindMountFlag    = uintptr(0)
	bindRemountFlag  = uintptr(0)
	bindReadOnlyFlag = uintptr(0)
)
