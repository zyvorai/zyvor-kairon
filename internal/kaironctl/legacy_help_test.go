// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// legacyVerbs are every DisableFlagParsing verb registered through legacyCmd
// plus the power verbs; none may reach the API for -h/--help.
var legacyVerbs = []string{
	"get", "describe", "delete", "edit", "scale", "claim", "fork", "import",
	"top", "trigger", "migrate", "evacuate", "recover", "cancel-migration",
	"fence", "snapshot", "restore", "backup", "backuppolicy", "dr", "mac",
	"volumes", "disk", "nic", "create", "start", "stop",
}

func runWithFakeAPI(t *testing.T, args ...string) (calls int32, err error) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, "unexpected API call", http.StatusTeapot)
	}))
	defer srv.Close()
	t.Setenv("KAIRON_KUBE_URL", srv.URL)
	t.Setenv("KUBECONFIG", "/nonexistent")
	root := NewRootCmd(nil)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err = root.Execute()
	return n.Load(), err
}

func TestLegacyHelpMakesNoAPICall(t *testing.T) {
	for _, verb := range legacyVerbs {
		for _, h := range []string{"-h", "--help"} {
			calls, err := runWithFakeAPI(t, verb, h)
			if err != nil {
				t.Errorf("%s %s: unexpected error %v", verb, h, err)
			}
			if calls != 0 {
				t.Errorf("%s %s: made %d API call(s)", verb, h, calls)
			}
		}
	}
}

func TestLegacyLeadingFlagRejectedBeforeAPICall(t *testing.T) {
	for _, verb := range []string{"migrate", "snapshot", "restore", "evacuate", "recover", "cancel-migration", "fence", "volumes", "disk", "nic", "scale", "edit", "trigger", "create", "import"} {
		calls, err := runWithFakeAPI(t, verb, "--anything")
		if err == nil {
			t.Errorf("%s --anything: expected an error", verb)
		}
		if calls != 0 {
			t.Errorf("%s --anything: made %d API call(s)", verb, calls)
		}
	}
}

func TestCheckLeadingPositional(t *testing.T) {
	if err := checkLeadingPositional("migrate", []string{"web-1", "--strategy", "cold"}); err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	if err := checkLeadingPositional("migrate", []string{"-x"}); err == nil {
		t.Fatal("leading flag accepted")
	}
	if err := checkLeadingPositional("get", []string{"-A"}); err != nil {
		t.Fatalf("get may start with flags: %v", err)
	}
}

func TestWantsHelp(t *testing.T) {
	for _, a := range [][]string{{"-h"}, {"--help"}, {"web-1", "--help"}} {
		if !wantsHelp(a) {
			t.Errorf("%v should want help", a)
		}
	}
	for _, a := range [][]string{nil, {"web-1"}, {"--", "-h"}} {
		if wantsHelp(a) {
			t.Errorf("%v should not want help", a)
		}
	}
}
