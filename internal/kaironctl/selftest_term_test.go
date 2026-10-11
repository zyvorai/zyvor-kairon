// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"encoding/pem"
	"github.com/zyvorai/kairon/internal/selftest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestDoctorAsChecksTreats403AsNotChecked(t *testing.T) {
	rep := doctorReport{Checks: []doctorCheck{
		{Name: "Controller", Status: checkFail, Detail: `kubernetes GET /apis/apps/v1/...: HTTP 403: {"message":"deployments.apps \"x\" is forbidden"}`, Hint: "x"},
		{Name: "Capable nodes", Status: checkFail, Detail: "no node carries the capable label"},
	}}
	got := doctorAsChecks(rep)
	if got[0].Status != selftest.Skip || !strings.Contains(got[0].Detail, "not checked") {
		t.Errorf("403 should be skipped, got %+v", got[0])
	}
	if got[1].Status != selftest.Fail {
		t.Errorf("a real failure must stay a failure, got %+v", got[1])
	}
}

func TestLiveStatusLineIsErasedAroundLogLinesAndNeverInPlainOutput(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1_000, 0)
	tr := &termReporter{w: &buf, start: now, now: func() time.Time { return now }, color: true, live: true, tickEvery: -1}

	tr.StepStart("boot", "Boot a CirrOS VM")
	if !strings.Contains(buf.String(), "boot  Boot a CirrOS VM") || !strings.Contains(buf.String(), "\r\033[2K") {
		t.Fatalf("expected a header and a drawn status line, got %q", buf.String())
	}
	before := buf.Len()
	tr.Log(selftest.LevelInfo, "creating the VM")
	got := buf.String()[before:]
	// Erase the status line, write the log line, then redraw the status.
	const erase = "\r\033[2K"
	logAt := strings.Index(got, "creating the VM")
	if !strings.HasPrefix(got, erase) || logAt < 0 {
		t.Fatalf("the status line must be erased before the log line: %q", got)
	}
	redraw := got[logAt:]
	if !strings.Contains(redraw, erase) || !strings.Contains(redraw, "boot  Boot a CirrOS VM") {
		t.Errorf("the status line must be redrawn after the log line: %q", got)
	}

	tr.StepDone(selftest.Result{ID: "boot", Status: selftest.Pass, Duration: time.Second})
	tr.Close()
	after := buf.String()
	if strings.LastIndex(after, "boot  Boot a CirrOS VM") > strings.LastIndex(after, "pass") {
		t.Errorf("the status line was redrawn after the step finished: %q", after)
	}
}

func TestNoStatusLineWhenNotLive(t *testing.T) {
	var buf bytes.Buffer
	tr := &termReporter{w: &buf, start: time.Now(), now: time.Now, color: true, live: false}
	tr.StepStart("boot", "Boot")
	tr.Log(selftest.LevelOK, "ok")
	tr.StepDone(selftest.Result{ID: "boot", Status: selftest.Pass})
	tr.Close()
	if strings.Contains(buf.String(), "\r") || strings.Contains(buf.String(), "\033[2K") {
		t.Errorf("a non-interactive writer must never get cursor control: %q", buf.String())
	}
}

func TestAnimationAdvancesTheSpinnerFrame(t *testing.T) {
	var buf bytes.Buffer
	tr := &termReporter{w: &buf, start: time.Now(), now: time.Now, color: false, live: true, tickEvery: 5 * time.Millisecond}
	tr.StepStart("boot", "Boot")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		f := tr.frame
		tr.mu.Unlock()
		if f >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	tr.Close()
	tr.mu.Lock()
	f := tr.frame
	tr.mu.Unlock()
	if f < 2 {
		t.Errorf("spinner never advanced (frame %d)", f)
	}
}

func TestUIHTTPClientTrustsAConfiguredCA(t *testing.T) {
	t.Setenv("KAIRON_UI_CA", "")
	t.Setenv("KAIRON_UI_INSECURE", "")
	if c, err := uiHTTPClient(); err != nil || c != http.DefaultClient {
		t.Fatalf("with nothing set the default client is used, got %v %v", c, err)
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()

	// Without the CA the self-signed server is rejected.
	if resp, err := http.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("default client must reject a self-signed certificate")
	}

	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAIRON_UI_CA", pemPath)
	c, err := uiHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("a client trusting KAIRON_UI_CA must connect: %v", err)
	}
	_ = resp.Body.Close()

	t.Setenv("KAIRON_UI_CA", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := uiHTTPClient(); err == nil {
		t.Error("a missing CA file must be an error, not silently ignored")
	}
	t.Setenv("KAIRON_UI_CA", "")
	t.Setenv("KAIRON_UI_INSECURE", "true")
	c, err = uiHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Get(srv.URL); err != nil {
		t.Errorf("KAIRON_UI_INSECURE must skip verification: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}
