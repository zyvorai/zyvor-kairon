// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"context"
	"strings"
	"testing"
)

func TestDiagnoseRanksPhaseConditionsEventsDrops(t *testing.T) {
	d := Diagnose(DiagnoseFacts{
		Kind: "Machine", Namespace: "ml", Name: "job-1", Phase: "Pending",
		Conditions: []FactCondition{
			{Type: "Scheduled", Status: "False", Reason: "Unschedulable", Message: "no node has 64 CPUs"},
			{Type: "Paused", Status: "False"},
		},
		Events: []FactEvent{
			{Type: "Warning", Reason: "EdgeDrop", Message: "dns_deny: ...", Count: 2},
			{Type: "Warning", Reason: "EdgeAnomaly", Message: "beacon"},
			{Type: "Warning", Reason: "EdgeDrop", Message: "dns_deny: ...", Count: 3},
			{Type: "Normal", Reason: "Scheduled"},
		},
		Drops: []Drop{{Reason: "sni_deny", SNI: "evil.example"}},
		Boot:  []BootFinding{{Code: "virtio_missing"}},
	})
	if d.Healthy || d.Apply || len(d.Causes) != 5 {
		t.Fatalf("%+v", d)
	}
	titles := make([]string, len(d.Causes))
	for i, c := range d.Causes {
		titles[i] = c.Title
	}
	if !strings.HasPrefix(titles[0], "condition Scheduled") || titles[1] != "Warning events: EdgeDrop (x5)" || !strings.HasPrefix(titles[2], "Warning events: EdgeAnomaly") || titles[3] != "edge drop: sni_deny" {
		t.Fatalf("order = %q", titles)
	}
	if d.Causes[0].Rank != 1 || len(d.Repair) != 1 || !strings.Contains(d.Summary, "most likely: condition Scheduled") {
		t.Fatalf("%+v", d)
	}
}

func TestDiagnoseFailedMigrationAndHealthy(t *testing.T) {
	d := Diagnose(DiagnoseFacts{Kind: "Migration", Name: "m1", Phase: "NeedsRecovery", Message: "both sides report running"})
	if len(d.Causes) != 1 || !strings.Contains(strings.Join(d.Causes[0].Propose, " "), "status.recovery") {
		t.Fatalf("%+v", d)
	}
	if h := Diagnose(DiagnoseFacts{Kind: "Machine", Name: "ok", Phase: "Running", Conditions: []FactCondition{
		{Type: "Ready", Status: "True"}, {Type: "NodeUnreachable", Status: "False", Reason: "NodeReady"},
		{Type: "Paused", Status: "True"}, {Type: "MemoryPressure", Status: "False"},
	}}); !h.Healthy || len(h.Causes) != 0 {
		t.Fatalf("healthy Machine flagged: %+v", h)
	}
	d = Diagnose(DiagnoseFacts{Kind: "Machine", Name: "cut", Phase: "Unknown", Conditions: []FactCondition{{Type: "NodeUnreachable", Status: "True", Reason: "NodeNotReady"}}})
	if len(d.Causes) != 1 || d.Causes[0].Title != "condition NodeUnreachable is True (NodeNotReady)" {
		t.Fatalf("%+v", d)
	}
}

func TestSummarizeKeepsDeterministicCauses(t *testing.T) {
	d := Diagnose(DiagnoseFacts{Kind: "Machine", Name: "x", Phase: "Failed", Message: "image digest mismatch"})
	chat := &scriptedChat{answers: []string{`{"summary":"The Machine failed because its image digest does not match."}`}}
	out, err := Summarize(context.Background(), chat, d)
	if err != nil || out.AISummary == "" || len(out.Causes) != len(d.Causes) {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.Contains(chat.seen[0][1].Content, "image digest mismatch") {
		t.Fatal("diagnosis not sent to the model")
	}
	if _, err := Summarize(context.Background(), &scriptedChat{answers: []string{`{}`}}, d); err == nil {
		t.Fatal("empty summary accepted")
	}
}
