// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package style

import "io"

// 256-colour palette tuned for a black terminal background (macOS Terminal's
// "Pro" and "Homebrew" profiles, iTerm, most Linux terminals). 256-colour
// escapes are understood everywhere the basic 16 are in practice, and
// everything still goes through Wrap, so NO_COLOR and --color=never apply.
const (
	Dim        = "\033[2m"
	BrightGrn  = "\033[1;38;5;83m"
	BrightCyan = "\033[1;38;5;87m"
	BrightYel  = "\033[1;38;5;221m"
	BrightRed  = "\033[1;38;5;203m"
	SoftGreen  = "\033[38;5;77m"
	SoftCyan   = "\033[38;5;80m"
	SoftYellow = "\033[38;5;179m"
	SoftRed    = "\033[38;5;167m"
	Grey       = "\033[38;5;245m"
	DarkGrey   = "\033[38;5;240m"
	White      = "\033[1;38;5;255m"
)

// SpinnerFrames is the braille spinner used by live status lines.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Paint is Wrap for the extended palette; it exists so callers read well.
func Paint(w io.Writer, color, s string) string { return Wrap(w, color, s) }
