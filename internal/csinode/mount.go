// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// IsMountPoint reports whether path is currently a mount target,
// checked by reading /proc/self/mountinfo directly -- pure Go, no
// `findmnt`/`mountpoint` binary needed. NodeStageVolume/NodePublishVolume
// (and their Unstage/Unpublish counterparts) use this to be idempotent,
// exactly as the CSI spec requires: calling either again against an
// already-correctly-mounted path MUST succeed without re-mounting, and
// calling Unstage/Unpublish against a path that's already unmounted MUST
// also succeed.
func IsMountPoint(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return isMountPointFromReader(f, path), nil
}

// isMountPointFromReader is IsMountPoint's pure parsing logic, split out
// so it's testable against an in-memory fixture instead of the real
// /proc/self/mountinfo (see mount_test.go).
func isMountPointFromReader(r io.Reader, path string) bool {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// mountinfo(5): field 5 (1-indexed) is the mount point, i.e.
		// index 4 after whitespace-splitting.
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 && unescapeMountinfo(fields[4]) == path {
			return true
		}
	}
	return false
}

// unescapeMountinfo decodes the octal escapes (\040 for space, \011 tab,
// \012 newline, \134 backslash) the kernel applies to mountinfo paths.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
