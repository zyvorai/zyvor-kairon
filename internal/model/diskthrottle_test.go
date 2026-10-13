// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestDiskThrottleValidate(t *testing.T) {
	ok := DiskThrottle{IOPSRead: 100, IOPSWrite: 50, BPSTotal: 1 << 20, BPSTotalMax: 2 << 20, BurstSeconds: 10}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*DiskThrottle){
		"total with read":       func(d *DiskThrottle) { d.IOPSTotal = 10 },
		"max below base":        func(d *DiskThrottle) { d.BPSTotalMax = 1 },
		"max without base":      func(d *DiskThrottle) { d.IOPSTotalMax = 10 },
		"burst without max":     func(d *DiskThrottle) { d.BPSTotalMax = 0 },
		"negative":              func(d *DiskThrottle) { d.IOPSRead = -1 },
		"over QEMU's max":       func(d *DiskThrottle) { d.BPSTotal, d.BPSTotalMax = DiskThrottleMax+1, DiskThrottleMax+1 },
		"burst seconds too big": func(d *DiskThrottle) { d.BurstSeconds = 86401 },
	} {
		bad := ok
		mut(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	var none *DiskThrottle
	if !none.IsZero() || !(&DiskThrottle{}).IsZero() || ok.IsZero() {
		t.Fatal("IsZero")
	}
}

func TestValidateDisksChecksThrottles(t *testing.T) {
	disks := []MachineDisk{{Name: "a", ClaimName: "c", IO: &DiskIO{Throttle: &DiskThrottle{IOPSTotal: 1, IOPSRead: 1}}}}
	if ValidateDisks(disks) == nil {
		t.Fatal("expected the bad throttle to be refused")
	}
	disks[0].IO = &DiskIO{Bus: "vhost-user-blk", Throttle: &DiskThrottle{IOPSTotal: 1}}
	if ValidateDisks(disks) == nil {
		t.Fatal("expected vhost-user-blk to be refused")
	}
	disks[0].IO.Bus = "virtio-blk"
	if err := ValidateDisks(disks); err != nil {
		t.Fatal(err)
	}
}
