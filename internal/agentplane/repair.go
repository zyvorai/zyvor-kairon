// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import "fmt"

// BootFinding is an offline GuestKit-style fact. ProposeRepair never
// mutates a disk.
type BootFinding struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

type RepairStep struct {
	Code   string `json:"code"`
	Action string `json:"action"`
	Apply  bool   `json:"apply"`
}

func ProposeRepair(findings []BootFinding) []RepairStep {
	out := make([]RepairStep, 0, len(findings))
	for _, f := range findings {
		step := RepairStep{Code: f.Code, Apply: false}
		switch f.Code {
		case "virtio_missing":
			step.Action = "inject virtio modules into the initramfs and set the boot disk to /dev/vda"
		case "vmware_tools":
			step.Action = "disable VMware tools services before first boot on KVM"
		case "disk_name":
			step.Action = "rewrite /dev/sdX references to /dev/vdX"
		case "ssh_disabled":
			step.Action = "enable sshd and install the declared authorized key via firstboot"
		case "cloud_init":
			step.Action = "rebuild cloud-init NoCloud seed; do not boot the imported image first"
		case "windows_virtio":
			step.Action = "offline-inject virtio-win drivers before the first Windows boot"
		default:
			step.Action = fmt.Sprintf("no compiled repair for %q; leave the disk untouched", f.Code)
		}
		if f.Detail != "" {
			step.Action += " (" + f.Detail + ")"
		}
		out = append(out, step)
	}
	return out
}
