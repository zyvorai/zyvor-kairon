// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"
)

func TestNetworkIdentityCommand(t *testing.T) {
	opts := &Options{Name: "kaironctl", Namespace: "demo", Color: "never"}
	root := NewRootCmd(opts)
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"network", "identity", "web"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) == "" {
		t.Fatalf("identity output = %q", buf.String())
	}
}

func TestNetworkCaptureRejectsLongWindow(t *testing.T) {
	opts := &Options{Name: "kaironctl", Namespace: "demo", Color: "never"}
	root := NewRootCmd(opts)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"network", "capture", "web", "--seconds", "31"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected capture over 30s to fail")
	}
}
