// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Write a valid chain without fsync per record; fixture creation is excluded
// from benchmarks, which measure reads rather than durable append latency.
func writeAuditFixture(tb testing.TB, count int) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "audit.jsonl")
	f, err := os.Create(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	prev := ""
	for i := 0; i < count; i++ {
		e, err := Record(Event{Principal: "operator", Tool: "create_claim", Claim: fmt.Sprintf("claim-%d", i), Diff: strings.Repeat("x", 512)})
		if err != nil {
			tb.Fatal(err)
		}
		e.PrevHash = prev
		e.Hash = chainHash(e)
		if err := enc.Encode(e); err != nil {
			tb.Fatal(err)
		}
		prev = e.Hash
	}
	return path
}

func TestFileLogFilteredReplayVerifiesUnmatchedRecords(t *testing.T) {
	path := writeAuditFixture(t, 100)
	l, err := OpenFileLog(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Replay("claim-0")
	if err != nil || len(got) != 1 || got[0].Claim != "claim-0" {
		t.Fatalf("replay = %v, %v", got, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"claim":"claim-99"`, `"claim":"tampered"`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if events, err := l.Replay("claim-0"); err == nil || events != nil {
		t.Fatalf("must reject corruption outside selected claim: %v, %v", events, err)
	}
}

func TestFileLogScanRejectsOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (4<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := VerifyFile(path); err == nil || n != 0 {
		t.Fatalf("verify = %d, %v", n, err)
	}
}

func BenchmarkFileLogRead(b *testing.B) {
	for _, count := range []int{100, 10000} {
		path := writeAuditFixture(b, count)
		for _, operation := range []string{"verify", "open", "replay-one", "replay-all"} {
			b.Run(fmt.Sprintf("%s/%d", operation, count), func(b *testing.B) {
				b.ReportAllocs()
				l := &FileLog{path: path}
				for i := 0; i < b.N; i++ {
					var err error
					switch operation {
					case "verify":
						_, err = VerifyFile(path)
					case "open":
						_, err = OpenFileLog(path)
					case "replay-one":
						_, err = l.Replay("claim-0")
					case "replay-all":
						_, err = l.Replay("")
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
