// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import (
	"testing"
	"time"
)

func TestFormatAndParseQuiesceRefRoundTrips(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ref := FormatQuiesceRef("my-snapshot", at)
	name, got, ok := ParseQuiesceRef(ref)
	if !ok {
		t.Fatalf("expected ParseQuiesceRef to succeed on %q", ref)
	}
	if name != "my-snapshot" {
		t.Fatalf("expected name %q, got %q", "my-snapshot", name)
	}
	if !got.Equal(at) {
		t.Fatalf("expected time %v, got %v", at, got)
	}
}

func TestParseQuiesceRefRejectsMalformedInput(t *testing.T) {
	cases := []string{"", "no-at-sign", "name@not-a-time"}
	for _, v := range cases {
		if _, _, ok := ParseQuiesceRef(v); ok {
			t.Fatalf("expected ParseQuiesceRef(%q) to fail", v)
		}
	}
}
