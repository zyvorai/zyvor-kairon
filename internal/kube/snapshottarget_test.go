// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import "testing"

func TestPickSnapshotTarget(t *testing.T) {
	vcs := []VolumeSnapshotClass{{Name: "rbd-snap", Driver: "rbd"}, {Name: "host-snap", Driver: "hostpath", Default: true}}
	scs := []StorageClass{
		{Name: "local-path", Provisioner: "local", Immediate: false, Default: true},
		{Name: "host-wffc", Provisioner: "hostpath", Immediate: false},
		{Name: "host-now", Provisioner: "hostpath", Immediate: true},
		{Name: "rbd-sc", Provisioner: "rbd", Immediate: true, Default: true},
	}
	got := PickSnapshotTarget(vcs, scs)
	// rbd-sc scores 4+2, host-now scores 4+1: the default StorageClass wins.
	if got.SnapshotClass != "rbd-snap" || got.StorageClass != "rbd-sc" || !got.ImmediateBinding {
		t.Errorf("picked %+v", got)
	}

	got = PickSnapshotTarget(vcs[1:], scs)
	if got.StorageClass != "host-now" {
		t.Errorf("Immediate binding must beat WaitForFirstConsumer, got %+v", got)
	}
}

func TestPickSnapshotTargetEdgeCases(t *testing.T) {
	if got := PickSnapshotTarget(nil, []StorageClass{{Name: "x", Provisioner: "p", Immediate: true}}); got != (SnapshotTarget{}) {
		t.Errorf("no classes should give the zero value, got %+v", got)
	}
	got := PickSnapshotTarget([]VolumeSnapshotClass{{Name: "c", Driver: "orphan"}}, []StorageClass{{Name: "x", Provisioner: "p", Immediate: true}})
	if got.SnapshotClass != "c" || got.StorageClass != "" || got.Driver != "orphan" {
		t.Errorf("a class with no matching StorageClass should be reported for the skip reason, got %+v", got)
	}
	got = PickSnapshotTarget([]VolumeSnapshotClass{{Name: "c", Driver: "d"}}, []StorageClass{{Name: "wffc", Provisioner: "d", Immediate: false}})
	if got.StorageClass != "wffc" || got.ImmediateBinding {
		t.Errorf("only a WaitForFirstConsumer class: got %+v", got)
	}
}
