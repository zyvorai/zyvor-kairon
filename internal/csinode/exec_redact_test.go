// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package csinode

import (
	"context"
	"strings"
	"testing"
)

func TestExecRunnerErrorDoesNotLeakCHAPSecrets(t *testing.T) {
	_, err := NewCommandRunner().Run(context.Background(), "false",
		"-n", "node.session.auth.password", "-v", "hunter2", "userid=bob", "password=hunter3")
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "hunter") {
		t.Fatalf("secret leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "userid=bob") {
		t.Fatalf("non-secret args should stay: %v", err)
	}
}

func TestISCSIIdentifiersRejectOptionAndPathInjection(t *testing.T) {
	good := map[string]string{"portal": "192.168.1.50:3260", "iqn": "iqn.2024-01.dev.zyvor:vol1"}
	if _, err := parseISCSIConfig(good, nil); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, bad := range []map[string]string{
		{"portal": "--op=delete", "iqn": good["iqn"]},
		{"portal": good["portal"], "iqn": "-I"},
		{"portal": good["portal"], "iqn": "iqn.x/../../backstores"},
		{"portal": "a b", "iqn": good["iqn"]},
	} {
		if _, err := parseISCSIConfig(bad, nil); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if _, err := decodeVolumeID("iscsi|--op=delete|iqn.a:b|0"); err == nil {
		t.Error("decodeVolumeID accepted option-like portal")
	}
	if err := ensureFormatted(context.Background(), nil, "/dev/x", "../../bin/sh"); err == nil {
		t.Error("ensureFormatted accepted a path-like fsType")
	}
}
