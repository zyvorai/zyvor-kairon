// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package macnode

import (
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// Discover reads this Mac's CPU count, unified memory, chip, macOS version and Thunderbolt Bridge.
func Discover() Info {
	mem, _ := unix.SysctlUint64("hw.memsize")
	arch := "arm64"
	if runtime.GOARCH == "amd64" {
		arch = "amd64"
	}
	i := Info{CPUs: runtime.NumCPU(), MemoryMiB: mem / (1024 * 1024), Arch: arch, OS: "darwin", InternalIP: PrimaryIPv4()}
	i.Chip, _ = unix.Sysctl("machdep.cpu.brand_string")
	i.Model, _ = unix.Sysctl("hw.model")
	i.MacOSVersion, _ = unix.Sysctl("kern.osproductversion")
	if p, err := unix.SysctlUint32("hw.perflevel0.physicalcpu"); err == nil {
		i.PerformanceCores = int(p)
	}
	if e, err := unix.SysctlUint32("hw.perflevel1.physicalcpu"); err == nil {
		i.EfficiencyCores = int(e)
	}
	if out, err := exec.Command("/usr/sbin/networksetup", "-listallhardwareports").Output(); err == nil {
		if dev := ThunderboltBridge(ParseHardwarePorts(string(out))); dev != "" {
			i.ThunderboltInterface = dev
			i.ThunderboltAddresses = InterfaceIPv4(dev)
		}
	}
	return i
}
