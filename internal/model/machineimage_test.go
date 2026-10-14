// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"strings"
	"testing"
)

const testDigest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func TestMachineImageValidate(t *testing.T) {
	disk := MachineImageSpec{Source: ImageSource{HTTPURL: "https://img/x.qcow2", Format: "qcow2"}, Digest: testDigest}
	if err := disk.Validate(); err != nil {
		t.Fatalf("disk image: %v", err)
	}
	iso := MachineImageSpec{Kind: "iso", Source: ImageSource{HTTPURL: "https://img/win.iso"}, Digest: testDigest}
	if err := iso.Validate(); err != nil {
		t.Fatalf("iso image: %v", err)
	}
	for name, bad := range map[string]MachineImageSpec{
		"unknown kind":  {Kind: "floppy", Source: ImageSource{HTTPURL: "https://x"}, Digest: testDigest},
		"iso format":    {Kind: "iso", Source: ImageSource{HTTPURL: "https://x", Format: "qcow2"}, Digest: testDigest},
		"iso oci":       {Kind: "iso", Source: ImageSource{OCI: "quay.io/x/y"}, Digest: testDigest},
		"iso repair":    {Kind: "iso", Source: ImageSource{HTTPURL: "https://x", Repair: true}, Digest: testDigest},
		"no digest":     {Source: ImageSource{HTTPURL: "https://x"}},
		"bad disk size": {Source: ImageSource{HTTPURL: "https://x"}, Digest: testDigest, Defaults: MachineImageDefaults{DiskSize: "lots"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := (MachineImageSpec{Source: ImageSource{HTTPURL: "https://x"}}).Validate(); err == nil || strings.Contains(err.Error(), "spec.image.") {
		t.Errorf("errors should name MachineImage fields, got %v", err)
	}
}

func TestValidateRootImage(t *testing.T) {
	ok := []ImageSpec{
		{},
		{Blank: true, DiskSize: "60Gi"},
		{ImageRef: "ubuntu-24-04", DiskSize: "40Gi"},
		{ImageRef: "ubuntu-24-04", Source: &ImageSource{HTTPURL: "https://x"}, Digest: testDigest},
	}
	for _, img := range ok {
		if err := ValidateRootImage(img); err != nil {
			t.Errorf("%+v: %v", img, err)
		}
	}
	bad := []ImageSpec{
		{Blank: true},
		{Blank: true, DiskSize: "60Gi", ImageRef: "x"},
		{Blank: true, DiskSize: "60Gi", Path: "/x"},
		{DiskSize: "huge"},
		{ImageRef: "x", CatalogName: "y"},
	}
	for _, img := range bad {
		if err := ValidateRootImage(img); err == nil {
			t.Errorf("%+v: want error", img)
		}
	}
}

func TestValidateCdroms(t *testing.T) {
	good := []MachineCdrom{
		{Name: "install", ImageRef: "windows-server-2022-iso"},
		{Name: "virtio", Source: &ImageSource{HTTPURL: "https://x/virtio-win.iso"}, Digest: testDigest},
	}
	if err := ValidateCdroms(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]MachineCdrom{
		"too many":   {{Name: "a", ImageRef: "x"}, {Name: "b", ImageRef: "x"}, {Name: "c", ImageRef: "x"}, {Name: "d", ImageRef: "x"}, {Name: "e", ImageRef: "x"}},
		"duplicate":  {{Name: "a", ImageRef: "x"}, {Name: "a", ImageRef: "y"}},
		"root":       {{Name: "root", ImageRef: "x"}},
		"no source":  {{Name: "a"}},
		"qcow2":      {{Name: "a", Source: &ImageSource{HTTPURL: "https://x", Format: "qcow2"}, Digest: testDigest}},
		"no digest":  {{Name: "a", Source: &ImageSource{HTTPURL: "https://x"}}},
		"bad scheme": {{Name: "a", Source: &ImageSource{HTTPURL: "ftp://x"}, Digest: testDigest}},
	}
	for name, cds := range cases {
		if err := ValidateCdroms(cds); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestValidateCdromsUpdate(t *testing.T) {
	old := []MachineCdrom{
		{Name: "install", ImageRef: "windows-server-2022-iso"},
		{Name: "virtio", Source: &ImageSource{HTTPURL: "https://x/virtio-win.iso"}, Digest: testDigest},
	}
	resolved := []MachineCdrom{
		{Name: "install", ImageRef: "windows-server-2022-iso", Source: &ImageSource{HTTPURL: "https://x/ws.iso"}, Digest: testDigest},
		old[1],
	}
	for name, updated := range map[string][]MachineCdrom{
		"unchanged":       old,
		"remove one":      old[1:],
		"remove all":      nil,
		"imageRef filled": resolved,
	} {
		if err := ValidateCdromsUpdate(old, updated); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, updated := range map[string][]MachineCdrom{
		"add":            append(append([]MachineCdrom(nil), old...), MachineCdrom{Name: "extra", ImageRef: "x"}),
		"swap imageRef":  {{Name: "install", ImageRef: "other-iso"}},
		"swap source":    {{Name: "virtio", Source: &ImageSource{HTTPURL: "https://x/other.iso"}, Digest: testDigest}},
		"rename install": {{Name: "setup", ImageRef: "windows-server-2022-iso"}},
	} {
		if err := ValidateCdromsUpdate(old, updated); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestValidateMachineCdromsUpdate(t *testing.T) {
	old := Machine{Spec: MachineSpec{PowerState: "Stopped"}}
	updated := old
	updated.Spec.Cdroms = []MachineCdrom{{Name: "tools", ImageRef: "virtio-win"}}
	if err := ValidateMachineCdromsUpdate(old, updated); err != nil {
		t.Fatalf("stopped machine: %v", err)
	}
	old.Status.RuntimeID = "vm-1"
	if err := ValidateMachineCdromsUpdate(old, updated); err == nil {
		t.Fatal("machine with a runtime accepted a new cdrom")
	}
	old.Status.RuntimeID, old.Spec.PowerState = "", "Halted"
	if err := ValidateMachineCdromsUpdate(old, updated); err == nil {
		t.Fatal("halted machine accepted a new cdrom")
	}
}

func TestParseDiskSizeGiB(t *testing.T) {
	for in, want := range map[string]uint64{"60Gi": 60, "1Ti": 1024, "1500Mi": 2, "100G": 94} {
		got, err := ParseDiskSizeGiB(in)
		if err != nil || got != want {
			t.Errorf("%s: got %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseDiskSizeGiB("big"); err == nil {
		t.Error("want error")
	}
}
