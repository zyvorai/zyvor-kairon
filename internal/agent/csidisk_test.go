// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/csinode"
	"github.com/zyvorai/kairon/internal/model"
)

func TestStoragePathsConditionCoversOwnNVMeVolumesOnThisNode(t *testing.T) {
	orig := nvmePathCondition
	t.Cleanup(func() { nvmePathCondition = orig })
	nvmePathCondition = func(id string) (bool, string, bool) {
		switch id {
		case "nvme|boot":
			return false, "2 of 2 paths usable", true
		case "nvme|data":
			return true, "1 of 2 paths usable", true
		}
		return false, "", false
	}
	a := &Agent{NodeName: "n1"}

	if _, ok := a.storagePathsCondition(model.MachineStatus{VolumeHandle: "iscsi|x|y|0"}); ok {
		t.Fatal("an iSCSI-only Machine got a StoragePaths condition")
	}
	cond, ok := a.storagePathsCondition(model.MachineStatus{VolumeHandle: "nvme|boot"})
	if !ok || cond.Status != "True" || cond.Reason != "AllPathsLive" {
		t.Fatalf("%+v", cond)
	}
	cond, ok = a.storagePathsCondition(model.MachineStatus{
		VolumeHandle: "nvme|boot",
		DiskVolumes: []model.DiskVolume{
			{Driver: csinode.DriverName, Node: "n1", VolumeHandle: "nvme|data"},
			{Driver: csinode.DriverName, Node: "other", VolumeHandle: "nvme|elsewhere"},
		},
	})
	if !ok || cond.Status != "False" || cond.Reason != "PathsDegraded" || !strings.Contains(cond.Message, "2 of 2 paths usable | 1 of 2 paths usable") {
		t.Fatalf("%+v", cond)
	}
	if _, ok := a.storagePathsCondition(model.MachineStatus{VolumeHandle: "nvme|boot", VolumeDriver: "rbd.csi.ceph.com"}); ok {
		t.Fatal("a third-party boot volume was checked")
	}
}

func TestRealBlockDeviceRejectsRegularFiles(t *testing.T) {
	f := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := realBlockDevice(f); err == nil || !strings.Contains(err.Error(), "not a block device") {
		t.Fatal(err)
	}
	if _, err := diskVolumePath(model.DiskVolume{Block: true, PublishPath: f}); err == nil {
		t.Fatal("a regular file passed as a Block volume")
	}
	if p, err := diskVolumePath(model.DiskVolume{PublishPath: "/pub/d"}); err != nil || p != "/pub/d/disk.img" {
		t.Fatal(p, err)
	}
}

func TestSeedBlockDeviceOnlyWritesABlankDevice(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "qemu-img")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := qemuImg
	qemuImg = fake
	t.Cleanup(func() { qemuImg = orig })

	qcow := filepath.Join(dir, "img.qcow2")
	if err := os.WriteFile(qcow, append([]byte("QFI\xfb"), make([]byte, 60)...), 0o600); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(dir, "dev")
	if err := os.WriteFile(dev, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedBlockDevice(dev, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("a blank Machine ran qemu-img")
	}
	if err := seedBlockDevice(dev, qcow); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "convert -n -O raw "+qcow+" "+dev {
		t.Fatalf("qemu-img %q", got)
	}
	used := make([]byte, 2<<20)
	used[4096] = 1
	if err := os.WriteFile(dev, used, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(log)
	if err := seedBlockDevice(dev, qcow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("a device holding data was overwritten")
	}
}

func TestSeedBlockDeviceCopiesARawImageWithoutQemuImg(t *testing.T) {
	dir := t.TempDir()
	orig := qemuImg
	qemuImg = filepath.Join(dir, "missing-qemu-img")
	t.Cleanup(func() { qemuImg = orig })

	raw := filepath.Join(dir, "img.raw")
	img := make([]byte, 3<<20+5)
	img[0], img[2<<20], img[len(img)-1] = 0xeb, 7, 9
	if err := os.WriteFile(raw, img, 0o600); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(dir, "dev")
	if err := os.WriteFile(dev, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedBlockDevice(dev, raw); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dev)
	if len(got) != 4<<20 || string(got[:len(img)]) != string(img) {
		t.Fatal("the raw image was not written onto the device")
	}

	qcow := filepath.Join(dir, "img.qcow2")
	if err := os.WriteFile(qcow, []byte("QFI\xfb"), 0o600); err != nil {
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "blank")
	if err := os.WriteFile(blank, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := seedBlockDevice(blank, qcow); err == nil || !strings.Contains(err.Error(), "raw image") {
		t.Fatalf("qcow2 without qemu-img: %v", err)
	}
}
