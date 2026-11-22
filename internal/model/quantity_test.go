// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import "testing"

func TestParseVCPUs(t *testing.T) {
	cases := map[string]uint32{"2": 2, "1500m": 2, "0.5": 1, "8": 8}
	for in, want := range cases {
		got, err := ParseVCPUs(in)
		if err != nil || got != want {
			t.Fatalf("ParseVCPUs(%q)=%d,%v want %d,nil", in, got, err, want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{"10Gi": 10 << 30, "1.5Ki": 1536, "20G": 20e9, "4096": 4096, "2k": 2000}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil || got != want {
			t.Fatalf("ParseBytes(%q)=%d,%v want %d,nil", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Gi", "-1Gi", "0", "lots", "99999999999Ti"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Fatalf("ParseBytes(%q) accepted", bad)
		}
	}
}

func TestParseMemoryMiB(t *testing.T) {
	cases := map[string]uint64{"2Gi": 2048, "512Mi": 512, "1G": 954, "1048576": 1}
	for in, want := range cases {
		got, err := ParseMemoryMiB(in)
		if err != nil || got != want {
			t.Fatalf("ParseMemoryMiB(%q)=%d,%v want %d,nil", in, got, err, want)
		}
	}
}
