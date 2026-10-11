// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zyvorai/kairon/internal/kaironctl/style"
	"github.com/zyvorai/kairon/internal/selftest"
)

// termReporter renders a self-test run as a dark-terminal log: a banner, one
// block per scenario with timestamped, level-coloured lines, and a result
// table. Colour follows style.Enabled, so NO_COLOR / --color=never give the
// same layout in plain text.
//
// On an interactive terminal it also keeps a live status line at the bottom
// (a braille spinner, the running step and its elapsed time). The line is
// erased before every other write and redrawn after it, so streamed log lines
// never collide with the animation. It is never drawn into a pipe or a file.
type termReporter struct {
	w     io.Writer
	start time.Time
	now   func() time.Time
	color bool
	step  string

	live      bool          // draw the animated status line
	tickEvery time.Duration // 0 means 120ms; negative disables the goroutine (tests)

	mu       sync.Mutex
	title    string // the running step, "" between steps
	stepAt   time.Time
	frame    int
	drawn    bool // a status line is currently on screen
	stop     chan struct{}
	stopOnce sync.Once
}

func newTermReporter(w io.Writer) *termReporter {
	color := style.Enabled(w)
	return &termReporter{
		w: w, start: time.Now(), now: time.Now, color: color,
		live: color && isTerminal(w) && os.Getenv("TERM") != "dumb",
	}
}

// isTerminal reports whether w is an interactive terminal (not a pipe/file).
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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

// out writes s with the status line erased around it.
func (t *termReporter) out(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearLocked()
	_, _ = io.WriteString(t.w, s)
	if t.live && t.title != "" {
		t.drawLocked()
	}
}

func (t *termReporter) clearLocked() {
	if t.drawn {
		_, _ = io.WriteString(t.w, "\r\033[2K")
		t.drawn = false
	}
}

func (t *termReporter) drawLocked() {
	frame := style.SpinnerFrames[t.frame%len(style.SpinnerFrames)]
	secs := t.now().Sub(t.stepAt).Seconds()
	line := fmt.Sprintf("  %s %s %s", t.paint(style.BrightCyan, frame), t.paint(style.Grey, t.title), t.paint(style.DarkGrey, fmt.Sprintf("%.0fs", secs)))
	_, _ = io.WriteString(t.w, "\r\033[2K"+line)
	t.drawn = true
}

// setStep records the running step and starts the animation if needed.
func (t *termReporter) setStep(title string) {
	t.mu.Lock()
	t.title, t.stepAt = title, t.now()
	if t.live {
		t.drawLocked()
		if t.stop == nil && t.tickEvery >= 0 {
			t.stop = make(chan struct{})
			every := t.tickEvery
			if every == 0 {
				every = 120 * time.Millisecond
			}
			go t.animate(every, t.stop)
		}
	}
	t.mu.Unlock()
}

func (t *termReporter) animate(every time.Duration, stop <-chan struct{}) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tk.C:
			t.mu.Lock()
			if t.title != "" {
				t.frame++
				t.drawLocked()
			}
			t.mu.Unlock()
		}
	}
}

// Close stops the animation and erases the status line.
func (t *termReporter) Close() {
	t.mu.Lock()
	t.title = ""
	t.clearLocked()
	stop := t.stop
	t.mu.Unlock()
	if stop != nil {
		t.stopOnce.Do(func() { close(stop) })
	}
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
	t.out("\n")
	t.out(" " + t.paint(style.DarkGrey, "╭"+rule+"╮") + "\n")
	t.out(" " + t.paint(style.DarkGrey, "│") + " " + t.paint(style.BrightGrn, title) + gap + t.paint(style.Grey, sub) + fill + " " + t.paint(style.DarkGrey, "│") + "\n")
	t.out(" " + t.paint(style.DarkGrey, "╰"+rule+"╯") + "\n")
	if len(meta) > 0 {
		t.out("  " + t.paint(style.Grey, strings.Join(meta, "  ·  ")) + "\n")
	}
	t.out("\n")
}

func (t *termReporter) StepStart(id, title string) {
	t.step = id
	t.mu.Lock()
	t.title = "" // keep the previous line from being redrawn under the header
	t.mu.Unlock()
	t.out(fmt.Sprintf("%s %s  %s\n", t.paint(style.BrightCyan, "▸"), t.paint(style.White, id), t.paint(style.Grey, title)))
	t.setStep(id + "  " + title)
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
		t.out(fmt.Sprintf("    %s %s %s\n", t.paint(style.DarkGrey, t.elapsed()), t.paint(style.DarkGrey, "vm│"), t.paint(style.Grey, msg)))
		return
	default:
		glyph, color = "·", style.SoftCyan
	}
	t.out(fmt.Sprintf("    %s %s %s\n", t.paint(style.DarkGrey, t.elapsed()), t.paint(color, glyph), msg))
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
	t.mu.Lock()
	t.title = "" // the step is over; stop drawing it
	t.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n", line)
	if r.Hint != "" {
		fmt.Fprintf(&b, "      %s %s\n", t.paint(style.BrightYel, "hint:"), r.Hint)
	}
	b.WriteString("\n")
	t.out(b.String())
}

// Summary prints the result table and the verdict.
func (t *termReporter) Summary(results []selftest.Result, strict bool) {
	t.Close()
	var b strings.Builder
	fmt.Fprintln(&b, t.paint(style.DarkGrey, strings.Repeat("─", 64)))
	fmt.Fprintf(&b, " %s\n", t.paint(style.Grey, fmt.Sprintf("%-8s %-16s %8s  %s", "RESULT", "SCENARIO", "TIME", "DETAIL")))
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
		fmt.Fprintf(&b, " %s %-16s %8s  %s\n", t.paint(color, fmt.Sprintf("%-8s", tag)), r.ID, r.Duration.Round(100*time.Millisecond), t.paint(style.Grey, detail))
	}
	s := selftest.Summarize(results)
	fmt.Fprintln(&b, t.paint(style.DarkGrey, strings.Repeat("─", 64)))
	counts := fmt.Sprintf("%d passed · %d failed · %d skipped", s.Pass, s.Fail, s.Skip)
	switch {
	case s.Fail > 0:
		fmt.Fprintf(&b, " %s  %s\n", t.paint(style.BrightRed, "✗ SOME CHECKS FAILED"), t.paint(style.Grey, counts))
	case strict && s.Skip > 0:
		fmt.Fprintf(&b, " %s  %s\n", t.paint(style.BrightYel, "! SKIPPED CHECKS (--strict)"), t.paint(style.Grey, counts))
	case s.Pass == 0:
		fmt.Fprintf(&b, " %s  %s\n", t.paint(style.BrightYel, "! NOTHING RAN"), t.paint(style.Grey, counts))
	default:
		fmt.Fprintf(&b, " %s  %s\n", t.paint(style.BrightGrn, "✓ KAIRON IS WORKING"), t.paint(style.Grey, counts))
	}
	_, _ = fmt.Fprintln(t.w)
	t.out(b.String())
}

// Compile-time check that the renderer satisfies the engine's interface.
var _ selftest.Reporter = (*termReporter)(nil)
