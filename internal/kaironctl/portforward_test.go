// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"strings"
	"testing"
)

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		in            string
		local, remote int
		wantErr       bool
	}{
		{"22", 22, 22, false},
		{"8080:80", 8080, 80, false},
		{"0:5432", 0, 5432, false},
		{"0", 0, 0, true},
		{"x:80", 0, 0, true},
		{"80:70000", 0, 0, true},
	}
	for _, tc := range cases {
		l, r, err := parsePortSpec(tc.in)
		if (err != nil) != tc.wantErr || l != tc.local || r != tc.remote {
			t.Errorf("parsePortSpec(%q) = %d, %d, %v", tc.in, l, r, err)
		}
	}
}

func TestSSHArgs(t *testing.T) {
	args, err := sshArgs("prod", "ubuntu@web-1", 2222, []string{"-i", "key"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "port-forward --stdio %h 2222") || !strings.Contains(joined, "-n prod") {
		t.Fatalf("missing ProxyCommand: %s", joined)
	}
	if !strings.Contains(joined, "HostKeyAlias=web-1.prod.kairon") {
		t.Fatalf("missing host key alias: %s", joined)
	}
	if args[len(args)-1] != "ubuntu@web-1" || args[len(args)-3] != "-i" {
		t.Fatalf("extra args must precede the destination: %v", args)
	}
}

func TestSCPArgs(t *testing.T) {
	args, err := scpArgs("default", 22, []string{"./a.txt", "ubuntu@web-1:/tmp/"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "HostKeyAlias=web-1.default.kairon") {
		t.Fatalf("args = %v", args)
	}
	args, err = scpArgs("default", 22, []string{"a:/x", "b:/y"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "HostKeyAlias") {
		t.Fatalf("two machines must not share an alias: %v", args)
	}
	if _, err := scpArgs("default", 22, []string{"./a", "./b"}); err == nil {
		t.Fatal("expected an error without a remote operand")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/usr/local/bin/kaironctl"); got != "/usr/local/bin/kaironctl" {
		t.Fatalf("plain path quoted: %s", got)
	}
	if got := shellQuote("/Users/a b/kaironctl"); got != "'/Users/a b/kaironctl'" {
		t.Fatalf("got %s", got)
	}
}
