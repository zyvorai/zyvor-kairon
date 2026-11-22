// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package model

import (
	"encoding/json"
	"testing"
	"time"
)

// TestMicroTimeMarshalsFixedSixDigitFractionalSeconds locks in the exact
// wire format a real Kubernetes API server requires for
// coordination.k8s.io/v1 LeaseSpec's acquireTime/renewTime
// (metav1.MicroTime) -- confirmed against a real cluster that a plain
// time.Time here (RFC3339Nano, variable/trimmed precision) gets rejected
// with "cannot parse ... as \"Z07:00\"" on every single Lease write.
func TestMicroTimeMarshalsFixedSixDigitFractionalSeconds(t *testing.T) {
	// A time whose nanosecond component would trim differently under
	// RFC3339Nano (85612926 ns trims to 9 digits, not the 6 required
	// here) -- the exact shape of value that broke the real API server.
	tm := NewMicroTime(time.Date(2026, 9, 14, 14, 50, 38, 85612926, time.UTC))
	b, err := json.Marshal(tm)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `"2026-09-14T14:50:38.085612Z"`
	if string(b) != want {
		t.Fatalf("MarshalJSON = %s, want %s", b, want)
	}
}

func TestMicroTimeRoundTrip(t *testing.T) {
	tm := NewMicroTime(time.Date(2026, 9, 14, 14, 50, 38, 85612000, time.UTC))
	b, err := json.Marshal(tm)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got MicroTime
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Equal(tm.Time) {
		t.Fatalf("round trip: got %v, want %v", got.Time, tm.Time)
	}
}

// TestMicroTimeUnmarshalAcceptsVariablePrecision confirms a real API
// server's own MicroTime output (always 6 digits) and this project's fake
// test doubles (which marshal via plain time.Time, RFC3339Nano) both parse
// correctly -- UnmarshalJSON deliberately doesn't require exactly 6
// digits back.
func TestMicroTimeUnmarshalAcceptsVariablePrecision(t *testing.T) {
	for _, s := range []string{
		`"2026-09-14T14:50:38.085612Z"`,
		`"2026-09-14T14:50:38.085612926Z"`,
		`"2026-09-14T14:50:38Z"`,
	} {
		var got MicroTime
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", s, err)
		}
	}
}
