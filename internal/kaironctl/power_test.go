// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestStopPatch(t *testing.T) {
	annotations := func(p map[string]any) map[string]any {
		md, _ := p["metadata"].(map[string]any)
		a, _ := md["annotations"].(map[string]any)
		return a
	}
	p, err := stopPatch("Stopped", false, 0)
	if err != nil || p["metadata"] != nil || p["spec"].(map[string]any)["powerState"] != "Stopped" {
		t.Fatalf("plain stop: %v %v", p, err)
	}
	p, _ = stopPatch("Halted", false, 45)
	if annotations(p)[model.AnnotationStopGracePeriod] != "45" {
		t.Fatalf("grace: %v", p)
	}
	p, _ = stopPatch("Stopped", true, 0)
	if v, ok := annotations(p)[model.AnnotationStopGracePeriod]; !ok || v != nil {
		t.Fatalf("--force must null the annotation: %v", p)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"metadata":{"annotations":{"kairon.zyvor.dev/stop-grace-period-seconds":null}},"spec":{"powerState":"Stopped"}}` {
		t.Fatalf("merge patch %s", b)
	}
	if _, err := stopPatch("Stopped", true, 10); err == nil {
		t.Fatal("--force with --grace-period must fail")
	}
	if _, err := stopPatch("Stopped", false, -1); err == nil {
		t.Fatal("negative grace must fail")
	}
}

func TestRestartPatchIsUniquePerCall(t *testing.T) {
	now := time.Date(2026, 10, 14, 6, 0, 0, 0, time.UTC)
	a := restartPatch(now)["metadata"].(map[string]any)["annotations"].(map[string]string)[model.AnnotationRestartRequested]
	b := restartPatch(now.Add(time.Millisecond))["metadata"].(map[string]any)["annotations"].(map[string]string)[model.AnnotationRestartRequested]
	if a == b || a != "2026-10-14T06:00:00Z" {
		t.Fatalf("a=%q b=%q", a, b)
	}
}
