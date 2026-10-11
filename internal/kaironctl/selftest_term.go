// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/selftest"
)

// termReporter renders a self-test run as a dark-terminal log: a banner, one
// block per scenario with timestamped, level-coloured lines, and a result
// table. Colour follows style.Enabled, so NO_COLOR / --color=never give the
// same layout in plain text.
type termReporter struct {
	w     io.Writer
	start time.Time
	now   func() time.Time
	color bool
	step  string
}

func newTermReporter(w io.Writer) *termReporter {
	return &termReporter{w: w, start: time.Now(), now: time.Now, color: style.Enabled(w)}
}

func (t *termReporter) paint(c, s string) string {
	if !t.color {
		return s
	}
	return c + s + style.Reset
}

func (t *termReporter) elapsed() string {
	return fmt.Sprintf("+%.1fs", t.now().Sub(t.start).Seconds())
}

// Banner prints the header box and a dim line of run metadata.
func (t *termReporter) Banner(profile selftest.Profile, meta ...string) {
	title := "KAIRON SELF-TEST"
	sub := "profile: " + string(profile)
	const gap = "    "
	width := utf8.RuneCountInString(title + gap + sub)
	if width < 44 {
		width = 44
	}
	fill := strings.Repeat(" ", width-utf8.RuneCountInString(title+gap+sub))
	rule := strings.Repeat("─", width+2)
	_, _ = fmt.Fprintln(t.w)
	_, _ = fmt.Fprintln(t.w, " "+t.paint(style.DarkGrey, "╭"+rule+"╮"))
	_, _ = fmt.Fprintln(t.w, " "+t.paint(style.DarkGrey, "│")+" "+t.paint(style.BrightGrn, title)+gap+t.paint(style.Grey, sub)+fill+" "+t.paint(style.DarkGrey, "│"))
	_, _ = fmt.Fprintln(t.w, " "+t.paint(style.DarkGrey, "╰"+rule+"╯"))
	if len(meta) > 0 {
		_, _ = fmt.Fprintln(t.w, "  "+t.paint(style.Grey, strings.Join(meta, "  ·  ")))
	}
	_, _ = fmt.Fprintln(t.w)
}

func (t *termReporter) StepStart(id, title string) {
	t.step = id
	_, _ = fmt.Fprintf(t.w, "%s %s  %s\n", t.paint(style.BrightCyan, "▸"), t.paint(style.White, id), t.paint(style.Grey, title))
}

func (t *termReporter) Log(l selftest.Level, msg string) {
	var glyph, color string
	switch l {
	case selftest.LevelOK:
		glyph, color = "✓", style.SoftGreen
	case selftest.LevelWarn:
		glyph, color = "!", style.BrightYel
	case selftest.LevelFail:
		glyph, color = "✗", style.BrightRed
	case selftest.LevelVM:
		_, _ = fmt.Fprintf(t.w, "    %s %s %s\n", t.paint(style.DarkGrey, t.elapsed()), t.paint(style.DarkGrey, "vm│"), t.paint(style.Grey, msg))
		return
	default:
		glyph, color = "·", style.SoftCyan
	}
	_, _ = fmt.Fprintf(t.w, "    %s %s %s\n", t.paint(style.DarkGrey, t.elapsed()), t.paint(color, glyph), msg)
}

func (t *termReporter) StepDone(r selftest.Result) {
	glyph, color, word := "✓", style.BrightGrn, "pass"
	switch r.Status {
	case selftest.Fail:
		glyph, color, word = "✗", style.BrightRed, "FAIL"
	case selftest.Skip:
		glyph, color, word = "–", style.BrightYel, "skip"
	}
	line := fmt.Sprintf("%s %s %s", t.paint(color, glyph), t.paint(color, word), t.paint(style.Grey, r.Duration.Round(100*time.Millisecond).String()))
	if r.Reason != "" {
		line += "  " + t.paint(color, r.Reason)
	}
	_, _ = fmt.Fprintf(t.w, "  %s\n", line)
	if r.Hint != "" {
		_, _ = fmt.Fprintf(t.w, "      %s %s\n", t.paint(style.BrightYel, "hint:"), r.Hint)
	}
	_, _ = fmt.Fprintln(t.w)
}

// Summary prints the result table and the verdict.
func (t *termReporter) Summary(results []selftest.Result, strict bool) {
	_, _ = fmt.Fprintln(t.w, t.paint(style.DarkGrey, strings.Repeat("─", 64)))
	_, _ = fmt.Fprintf(t.w, " %s\n", t.paint(style.Grey, fmt.Sprintf("%-8s %-16s %8s  %s", "RESULT", "SCENARIO", "TIME", "DETAIL")))
	for _, r := range results {
		tag, color := "✓ PASS", style.BrightGrn
		switch r.Status {
		case selftest.Fail:
			tag, color = "✗ FAIL", style.BrightRed
		case selftest.Skip:
			tag, color = "– SKIP", style.BrightYel
		}
		detail := r.Reason
		if len(detail) > 70 {
			detail = detail[:69] + "…"
		}
		_, _ = fmt.Fprintf(t.w, " %s %-16s %8s  %s\n", t.paint(color, fmt.Sprintf("%-8s", tag)), r.ID, r.Duration.Round(100*time.Millisecond), t.paint(style.Grey, detail))
	}
	s := selftest.Summarize(results)
	_, _ = fmt.Fprintln(t.w, t.paint(style.DarkGrey, strings.Repeat("─", 64)))
	counts := fmt.Sprintf("%d passed · %d failed · %d skipped", s.Pass, s.Fail, s.Skip)
	switch {
	case s.Fail > 0:
		_, _ = fmt.Fprintf(t.w, " %s  %s\n", t.paint(style.BrightRed, "✗ SOME CHECKS FAILED"), t.paint(style.Grey, counts))
	case strict && s.Skip > 0:
		_, _ = fmt.Fprintf(t.w, " %s  %s\n", t.paint(style.BrightYel, "! SKIPPED CHECKS (--strict)"), t.paint(style.Grey, counts))
	case s.Pass == 0:
		_, _ = fmt.Fprintf(t.w, " %s  %s\n", t.paint(style.BrightYel, "! NOTHING RAN"), t.paint(style.Grey, counts))
	default:
		_, _ = fmt.Fprintf(t.w, " %s  %s\n", t.paint(style.BrightGrn, "✓ KAIRON IS WORKING"), t.paint(style.Grey, counts))
	}
	_, _ = fmt.Fprintln(t.w)
}

// Compile-time check that the renderer satisfies the engine's interface.
var _ selftest.Reporter = (*termReporter)(nil)
