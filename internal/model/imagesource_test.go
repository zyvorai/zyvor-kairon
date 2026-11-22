// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"strings"
	"testing"
)

func TestParseOCIReference(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		in   string
		want OCIReference
	}{
		{"fedora", OCIReference{Registry: "registry-1.docker.io", Repository: "library/fedora"}},
		{"kubevirt/fedora:40", OCIReference{Registry: "registry-1.docker.io", Repository: "kubevirt/fedora", Tag: "40"}},
		{"quay.io/containerdisks/fedora:40", OCIReference{Registry: "quay.io", Repository: "containerdisks/fedora", Tag: "40"}},
		{"localhost:5000/disks/alpine@" + d, OCIReference{Registry: "localhost:5000", Repository: "disks/alpine", Digest: d}},
		{"ghcr.io/a/b/c:v1@" + d, OCIReference{Registry: "ghcr.io", Repository: "a/b/c", Tag: "v1", Digest: d}},
	}
	for _, c := range cases {
		got, err := ParseOCIReference(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseOCIReference(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "https://quay.io/x", "quay.io/X/Y", "quay.io/x@sha256:short", "quay.io/x:", "quay.io/../x", "a b"} {
		if _, err := ParseOCIReference(bad); err == nil {
			t.Errorf("ParseOCIReference(%q): expected an error", bad)
		}
	}
}
