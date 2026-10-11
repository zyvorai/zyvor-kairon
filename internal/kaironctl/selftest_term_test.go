// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/selftest"
)

func sampleRun(t *termReporter) []selftest.Result {
	t.Banner(selftest.ProfileStandard, "run abc123", "namespace default")
	t.StepStart("boot", "Boot a CirrOS VM")
	t.Log(selftest.LevelInfo, "creating default/kairon-selftest-abc123-vm")
	t.Log(selftest.LevelOK, "Running on node worker-1 after 12s")
	t.Log(selftest.LevelVM, "cirros login:")
	t.Log(selftest.LevelWarn, "serial console is slow")
	ok := selftest.Result{ID: "boot", Title: "Boot a CirrOS VM", Status: selftest.Pass, Duration: 12400 * time.Millisecond}
	t.StepDone(ok)
	t.StepStart("network", "Guest network")
	bad := selftest.Result{ID: "network", Status: selftest.Fail, Reason: "no SSH banner", Hint: "check forwards", Duration: time.Second}
	t.StepDone(bad)
	skip := selftest.Result{ID: "nic-hotplug", Status: selftest.Skip, Reason: "pass --bridge NAME"}
	t.StepDone(skip)
	res := []selftest.Result{ok, bad, skip}
	t.Summary(res, false)
	return res
}

func TestTermReporterPlainLayout(t *testing.T) {
	var buf bytes.Buffer
	tr := &termReporter{w: &buf, start: time.Now(), now: time.Now} // color false
	sampleRun(tr)
	out := buf.String()
	if strings.Contains(out, "\033[") {
		t.Errorf("plain output contains ANSI escapes:\n%q", out)
	}
	for _, want := range []string{
		"KAIRON SELF-TEST", "profile: standard", "run abc123",
		"▸ boot", "Running on node worker-1", "vm│ cirros login:",
		"✓ pass", "✗ FAIL", "no SSH banner", "hint: check forwards", "– skip",
		"✓ PASS", "✗ FAIL", "– SKIP", "1 passed · 1 failed · 1 skipped", "SOME CHECKS FAILED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestTermReporterColorAndVerdicts(t *testing.T) {
	var buf bytes.Buffer
	tr := &termReporter{w: &buf, start: time.Now(), now: time.Now, color: true}
	sampleRun(tr)
	if !strings.Contains(buf.String(), "\033[") {
		t.Error("color output has no ANSI escapes")
	}

	var all bytes.Buffer
	good := &termReporter{w: &all, start: time.Now(), now: time.Now}
	good.Summary([]selftest.Result{{ID: "boot", Status: selftest.Pass}}, false)
	if !strings.Contains(all.String(), "KAIRON IS WORKING") {
		t.Errorf("all-pass verdict missing:\n%s", all.String())
	}
	var strict bytes.Buffer
	st := &termReporter{w: &strict, start: time.Now(), now: time.Now}
	st.Summary([]selftest.Result{{ID: "boot", Status: selftest.Pass}, {ID: "x", Status: selftest.Skip}}, true)
	if !strings.Contains(strict.String(), "SKIPPED CHECKS (--strict)") {
		t.Errorf("strict verdict missing:\n%s", strict.String())
	}
}

func TestDoctorAsChecksMapsStatuses(t *testing.T) {
	rep := doctorReport{Checks: []doctorCheck{
		{Name: "a", Status: checkOK, Detail: "fine"},
		{Name: "b", Status: checkWarn, Detail: "meh"},
		{Name: "c", Status: checkFail, Detail: "bad", Hint: "fix it"},
		{Name: "d", Status: checkSkip, Detail: "n/a"},
	}}
	got := doctorAsChecks(rep)
	if len(got) != 4 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Status != selftest.Pass || got[0].Warn {
		t.Errorf("ok mapped to %+v", got[0])
	}
	if got[1].Status != selftest.Pass || !got[1].Warn {
		t.Errorf("warn mapped to %+v", got[1])
	}
	if got[2].Status != selftest.Fail || got[2].Hint != "fix it" {
		t.Errorf("fail mapped to %+v", got[2])
	}
	if got[3].Status != selftest.Skip {
		t.Errorf("skip mapped to %+v", got[3])
	}
}

func TestListSelftestShowsEveryScenario(t *testing.T) {
	var buf bytes.Buffer
	listSelftest(&buf)
	for _, id := range selftest.IDs() {
		if !strings.Contains(buf.String(), id) {
			t.Errorf("--list missing %s", id)
		}
	}
}
