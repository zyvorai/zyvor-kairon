// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package style provides Cilium-style ANSI colors and emoji progress logging
// for kaironctl. Colors are raw ANSI (no third-party color library); they
// respect TTY detection, NO_COLOR, and --color=auto|always|never.
package style

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	Reset   = "\033[0m"
	Red     = "\033[31m"
	Green   = "\033[32m"
	Yellow  = "\033[33m"
	Blue    = "\033[34m"
	Magenta = "\033[35m"
	Cyan    = "\033[36m"
	Bold    = "\033[1m"
)

// Emoji vocabulary (Cilium-shaped).
const (
	EmojiDetect  = "🔮"
	EmojiInfo    = "ℹ️"
	EmojiSparkle = "✨"
	EmojiRocket  = "🚀"
	EmojiWait    = "⌛"
	EmojiOK      = "✅"
	EmojiFire    = "🔥"
	EmojiWarn    = "⚠️"
	EmojiFail    = "❌"
	EmojiSkip    = "⏭️"
)

// ColorMode controls when ANSI is emitted.
type ColorMode int

const (
	ColorAuto ColorMode = iota
	ColorAlways
	ColorNever
)

var (
	mu    sync.Mutex
	mode            = ColorAuto
	outW  io.Writer = os.Stdout
	errW  io.Writer = os.Stderr
	quiet bool      // mute emoji progress (e.g. --dry-run)
)

// SetColorMode configures color emission for the process.
func SetColorMode(m ColorMode) {
	mu.Lock()
	defer mu.Unlock()
	mode = m
}

// ParseColorMode parses auto|always|never (case-insensitive).
func ParseColorMode(s string) (ColorMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ColorAuto, nil
	case "always", "on", "true", "1", "force":
		return ColorAlways, nil
	case "never", "off", "false", "0":
		return ColorNever, nil
	default:
		return ColorAuto, fmt.Errorf("invalid --color %q: want auto|always|never", s)
	}
}

// SetQuiet mutes emoji progress lines (install --dry-run).
func SetQuiet(q bool) {
	mu.Lock()
	defer mu.Unlock()
	quiet = q
}

// Enabled reports whether ANSI colors should be written to w.
func Enabled(w io.Writer) bool {
	mu.Lock()
	m := mode
	mu.Unlock()
	if os.Getenv("NO_COLOR") != "" && m != ColorAlways {
		return false
	}
	switch m {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default:
		f, ok := w.(*os.File)
		if !ok {
			return false
		}
		fi, err := f.Stat()
		if err != nil {
			return false
		}
		return (fi.Mode() & os.ModeCharDevice) != 0
	}
}

// Wrap wraps s in ANSI color when Enabled(w).
func Wrap(w io.Writer, color, s string) string {
	if !Enabled(w) || color == "" {
		return s
	}
	return color + s + Reset
}

// Phase colors a resource phase cell for tables.
func Phase(w io.Writer, phase string) string {
	p := strings.TrimSpace(phase)
	if p == "" || p == "-" {
		return dash(p)
	}
	var c string
	switch p {
	case "Running", "Succeeded", "Completed", "Ready", "Available":
		c = Green
	case "Pending", "Starting", "Scheduling", "Preparing", "Migrating", "Cutover", "Creating", "Updating":
		c = Yellow
	case "Failed", "Error", "NeedsRecovery", "Unknown":
		c = Red
	case "Paused", "Halted", "Hibernated", "Stopped", "Cancelled", "Suspended":
		c = Cyan
	default:
		c = ""
	}
	return Wrap(w, c, p)
}

// BoolReady colors a boolean ready column.
func BoolReady(w io.Writer, ready bool) string {
	if ready {
		return Wrap(w, Green, "true")
	}
	return Wrap(w, Yellow, "false")
}

// Header wraps a section header for describe previews.
func Header(w io.Writer, s string) string {
	return Wrap(w, Bold+Cyan, s)
}

// Success formats a kubectl-style mutation line, with ✅ when colored.
func Success(w io.Writer, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if Enabled(w) {
		return EmojiOK + " " + msg
	}
	return msg
}

// Log writes an emoji progress line to stdout unless quiet.
func Log(emoji, format string, args ...any) {
	mu.Lock()
	q := quiet
	w := outW
	mu.Unlock()
	if q {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if emoji != "" {
		_, _ = fmt.Fprintf(w, "%s %s\n", emoji, msg)
	} else {
		_, _ = fmt.Fprintln(w, msg)
	}
}

// Failf writes a failure line to stderr.
func Failf(format string, args ...any) {
	mu.Lock()
	w := errW
	mu.Unlock()
	_, _ = fmt.Fprintf(w, "%s %s\n", EmojiFail, fmt.Sprintf(format, args...))
}

// Warnf writes a warning line to stderr.
func Warnf(format string, args ...any) {
	mu.Lock()
	w := errW
	mu.Unlock()
	_, _ = fmt.Fprintf(w, "%s %s\n", EmojiWarn, fmt.Sprintf(format, args...))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// BrandMark is a small ASCII mark for status output.
func BrandMark() string {
	return ` _  __     _
| |/ /__ _(_)_ __ ___  _ __
| ' // _' | | '__/ _ \| '_ \
| . \ (_| | | | | (_) | | | |
|_|\_\__,_|_|_|  \___/|_| |_|`
}

// ClearLines moves the cursor up n lines and clears them (interactive status).
func ClearLines(w io.Writer, n int) {
	for i := 0; i < n; i++ {
		_, _ = fmt.Fprint(w, "\033[A\033[2K")
	}
}
