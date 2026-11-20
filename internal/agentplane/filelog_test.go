// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileLogChainsAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.jsonl")
	log, err := OpenFileLog(path)
	if err != nil {
		t.Fatal(err)
	}
	var mirrored []string
	log.Mirror = func(e Event) error { mirrored = append(mirrored, e.ID); return nil }
	first, err := log.Append(Event{Principal: "hermes", Tool: "create_claim", Claim: "job-1", Outcome: "intent"})
	if err != nil || first.Hash == "" || first.PrevHash != "" {
		t.Fatalf("%+v %v", first, err)
	}
	if _, err := log.Append(Event{Principal: "hermes", Tool: "create_claim", Claim: "job-1", Outcome: "ok"}); err != nil {
		t.Fatal(err)
	}
	if len(mirrored) != 2 {
		t.Fatalf("mirror saw %d", len(mirrored))
	}

	reopened, err := OpenFileLog(path)
	if err != nil {
		t.Fatal(err)
	}
	third, err := reopened.Append(Event{Principal: "op", Tool: "release_claim", Claim: "job-2"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.Replay("job-1")
	if err != nil || len(events) != 2 || events[1].Outcome != "ok" {
		t.Fatalf("replay = %+v %v", events, err)
	}
	if third.PrevHash != events[1].Hash {
		t.Fatal("reopened log did not continue the chain")
	}
	if n, err := VerifyFile(path); err != nil || n != 3 {
		t.Fatalf("verify = %d %v", n, err)
	}
}

func TestFileLogDetectsTamperingAndRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, _ := OpenFileLog(path)
	for _, claim := range []string{"a", "b", "c"} {
		if _, err := log.Append(Event{Principal: "p", Tool: "t", Claim: claim}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	edited := strings.Replace(string(raw), `"claim":"b"`, `"claim":"x"`, 1)
	_ = os.WriteFile(path, []byte(edited), 0o600)
	if _, err := VerifyFile(path); err == nil {
		t.Fatal("edited record passed verification")
	}

	_ = os.WriteFile(path, []byte(lines[0]+"\n"+lines[2]+"\n"), 0o600)
	if _, err := VerifyFile(path); err == nil {
		t.Fatal("removed record passed verification")
	}
	if _, err := OpenFileLog(path); err == nil {
		t.Fatal("open must refuse a broken chain")
	}
}
