// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"strings"
	"testing"
)

// A trimmed, realistic /proc/self/mountinfo fixture -- field 5 (index 4
// after whitespace-splitting) is the mount point, per mountinfo(5).
const mountinfoFixture = `36 35 98:0 / / rw,noatime shared:1 - ext4 /dev/root rw,errors=remount-ro
37 36 0:31 / /var/lib/kairon/csi/staging/pv-1 rw,relatime shared:2 - ext4 /dev/sda rw
38 36 0:31 / /var/lib/kairon/csi/publish/pv-1 rw,relatime shared:2 - ext4 /dev/sda rw
`

func TestIsMountPointFromReaderFindsExactMatch(t *testing.T) {
	if !isMountPointFromReader(strings.NewReader(mountinfoFixture), "/var/lib/kairon/csi/staging/pv-1") {
		t.Fatal("expected the staging path to be found as a mount point")
	}
	if !isMountPointFromReader(strings.NewReader(mountinfoFixture), "/var/lib/kairon/csi/publish/pv-1") {
		t.Fatal("expected the publish path to be found as a mount point")
	}
}

func TestIsMountPointFromReaderRejectsUnmountedPath(t *testing.T) {
	if isMountPointFromReader(strings.NewReader(mountinfoFixture), "/var/lib/kairon/csi/staging/pv-2") {
		t.Fatal("expected an unmounted path not to be reported as a mount point")
	}
}

func TestIsMountPointFromReaderRejectsPrefixMatch(t *testing.T) {
	// A naive substring/prefix check would wrongly match here -- this
	// must compare the whole field, not just check containment.
	if isMountPointFromReader(strings.NewReader(mountinfoFixture), "/var/lib/kairon/csi/staging/pv-1/extra") {
		t.Fatal("expected a path that only shares a prefix with a real mount point not to match")
	}
}

func TestIsMountPointFromReaderHandlesEmptyInput(t *testing.T) {
	if isMountPointFromReader(strings.NewReader(""), "/anything") {
		t.Fatal("expected no match against empty mountinfo")
	}
}

func TestIsMountPointFromReaderDecodesOctalEscapes(t *testing.T) {
	fixture := `40 36 0:31 / /var/lib/kubelet/my\040vol rw,relatime shared:2 - ext4 /dev/sda rw
`
	if !isMountPointFromReader(strings.NewReader(fixture), "/var/lib/kubelet/my vol") {
		t.Fatal("expected an escaped space in mountinfo to match the real path")
	}
	if isMountPointFromReader(strings.NewReader(fixture), `/var/lib/kubelet/my\040vol`) {
		t.Fatal("literal escape text must not match")
	}
}
