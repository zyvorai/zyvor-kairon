// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package macnode

import (
	"bufio"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Discover on a Linux host with no kubelet: memory is MemTotal from
// /proc/meminfo, or KAIRON_MAC_MEMORY_MIB when set (development).
func Discover() Info {
	mib, _ := strconv.ParseUint(os.Getenv("KAIRON_MAC_MEMORY_MIB"), 10, 64)
	if mib == 0 {
		if f, err := os.Open("/proc/meminfo"); err == nil {
			mib = memTotalMiB(f)
			_ = f.Close()
		}
	}
	return Info{CPUs: runtime.NumCPU(), MemoryMiB: mib, Arch: runtime.GOARCH, OS: runtime.GOOS, InternalIP: PrimaryIPv4()}
}

// memTotalMiB reads MemTotal (in kB) from /proc/meminfo content.
func memTotalMiB(r io.Reader) uint64 {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb / 1024
		}
	}
	return 0
}
