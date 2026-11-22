// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package style

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestParseColorMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ColorMode
		err  bool
	}{
		{"auto", ColorAuto, false},
		{"ALWAYS", ColorAlways, false},
		{"never", ColorNever, false},
		{"bogus", ColorAuto, true},
	} {
		got, err := ParseColorMode(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseColorMode(%q) want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseColorMode(%q) = %v, %v want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestPhaseColors(t *testing.T) {
	SetColorMode(ColorAlways)
	defer SetColorMode(ColorAuto)
	var buf bytes.Buffer
	// Enabled checks *os.File; force via ColorAlways which ignores writer type...
	// Wrap still needs Enabled(w); ColorAlways returns true regardless of writer.
	got := Phase(&buf, "Running")
	if !strings.Contains(got, Green) || !strings.Contains(got, "Running") {
		t.Errorf("Phase(Running) = %q, want green", got)
	}
	got = Phase(&buf, "Failed")
	if !strings.Contains(got, Red) {
		t.Errorf("Phase(Failed) = %q, want red", got)
	}
	got = Phase(&buf, "Paused")
	if !strings.Contains(got, Cyan) {
		t.Errorf("Phase(Paused) = %q, want cyan", got)
	}
	SetColorMode(ColorNever)
	got = Phase(&buf, "Running")
	if got != "Running" {
		t.Errorf("Phase with ColorNever = %q, want plain", got)
	}
}

func TestLogQuiet(t *testing.T) {
	var buf bytes.Buffer
	mu.Lock()
	old := outW
	outW = &buf
	mu.Unlock()
	defer func() {
		SetQuiet(false)
		mu.Lock()
		outW = old
		mu.Unlock()
	}()
	SetQuiet(true)
	Log(EmojiOK, "hello")
	if buf.Len() != 0 {
		t.Fatalf("quiet log wrote %q", buf.String())
	}
	SetQuiet(false)
	Log(EmojiOK, "hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestNOColorEnv(t *testing.T) {
	SetColorMode(ColorAuto)
	t.Setenv("NO_COLOR", "1")
	if Enabled(os.Stdout) {
		t.Fatal("NO_COLOR should disable colors in auto mode")
	}
	SetColorMode(ColorAlways)
	if !Enabled(os.Stdout) {
		t.Fatal("ColorAlways should override NO_COLOR")
	}
	SetColorMode(ColorAuto)
}
