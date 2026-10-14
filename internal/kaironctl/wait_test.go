// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseWaitFor(t *testing.T) {
	ok := map[string]waitCond{
		"phase=Running": {Kind: "phase", Phase: "Running"},
		"ip":            {Kind: "ip"},
		"agent":         {Kind: "agent"},
		"ssh":           {Kind: "ssh", Port: 22},
		"ssh=2222":      {Kind: "ssh", Port: 2222},
		"port=8080":     {Kind: "port", Port: 8080},
		"delete":        {Kind: "delete"},
	}
	for in, want := range ok {
		got, err := parseWaitFor(in)
		if err != nil || got != want {
			t.Fatalf("%q: %+v %v", in, got, err)
		}
		if in != "phase=Running" && got.String() != in {
			t.Fatalf("String %q != %q", got.String(), in)
		}
	}
	for _, bad := range []string{"", "phase=", "port", "port=0", "ssh=x", "agent=1", "ready"} {
		if _, err := parseWaitFor(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestWaitLoop(t *testing.T) {
	n := 0
	var states []string
	err := waitLoop(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool, string, error) {
		n++
		switch {
		case n < 3:
			return false, "phase Starting", nil
		case n < 5:
			return false, "", fmt.Errorf("transient")
		}
		return true, "Running", nil
	}, func(s string) { states = append(states, s) })
	if err != nil || n != 5 || strings.Join(states, ",") != "phase Starting,transient" {
		t.Fatalf("err=%v n=%d states=%v", err, n, states)
	}

	err = waitLoop(context.Background(), 30*time.Millisecond, time.Millisecond, func(context.Context) (bool, string, error) {
		return false, "phase Pending", nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "phase Pending") {
		t.Fatalf("timeout: %v", err)
	}

	err = waitLoop(context.Background(), time.Second, time.Millisecond, func(context.Context) (bool, string, error) {
		return false, "", fmt.Errorf("%w: machine/x is Failed", errWaitAbort)
	}, nil)
	if !errors.Is(err, errWaitAbort) {
		t.Fatalf("abort: %v", err)
	}
}

func TestSSHBanner(t *testing.T) {
	if !sshBanner(strings.NewReader("SSH-2.0-OpenSSH_9.6\r\n")) || sshBanner(strings.NewReader("HTTP/1.1 400\r\n")) || sshBanner(strings.NewReader("")) {
		t.Fatal("sshBanner")
	}
}

func TestDashboardMachineURL(t *testing.T) {
	if got := dashboardMachineURL("https://ui.example/", "prod", "web 1"); got != "https://ui.example/#/machines/prod/web%201" {
		t.Fatal(got)
	}
}
