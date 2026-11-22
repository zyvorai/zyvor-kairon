// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func steady(i int) Sample {
	return Sample{Bytes: 100000 + float64(i%5)*1000, Flows: 10 + float64(i%3), Dsts: 3, SNIs: 2, DenyRate: 0}
}

func TestBaselineFlagsOnlyAfterWarmup(t *testing.T) {
	b := NewBaseline()
	key := BaselineKey("acme", "ml", "job-1")
	spike := Sample{Bytes: 50_000_000, Flows: 11, Dsts: 3, SNIs: 2}
	for i := 0; i < b.Warmup-1; i++ {
		b.Observe(key, steady(i))
	}
	if f := b.Observe(key, spike); len(f) != 0 {
		t.Fatalf("flagged during warm-up: %+v", f)
	}
	for i := 0; i < 20; i++ {
		if f := b.Observe(key, steady(i)); len(f) != 0 {
			t.Fatalf("steady traffic flagged: %+v", f)
		}
	}
	f := b.Observe(key, spike)
	if len(f) != 1 || f[0].Kind != "baseline_deviation" || !strings.HasPrefix(f[0].Summary, "bytes ") || f[0].Apply {
		t.Fatalf("%+v", f)
	}
	if other := b.Observe(BaselineKey("other", "ml", "job-1"), spike); len(other) != 0 {
		t.Fatal("tenants must not share a baseline")
	}
}

func TestBaselineMinStdAndDenyRate(t *testing.T) {
	b := &Baseline{Alpha: 0.1, K: 4, Warmup: 5}
	for i := 0; i < 10; i++ {
		b.Observe("k", Sample{Dsts: 1, Flows: 4})
	}
	if f := b.Observe("k", Sample{Dsts: 2, Flows: 4}); len(f) != 0 {
		t.Fatalf("one extra destination is not an anomaly: %+v", f)
	}
	s := SampleFrom([]Flow{{DstIP: "a"}}, []Drop{{Reason: "dns_deny", Count: 9}})
	if s.DenyRate != 0.9 || s.Dsts != 1 {
		t.Fatalf("%+v", s)
	}
	f := b.Observe("k", s)
	if len(f) == 0 || !strings.Contains(f[len(f)-1].Summary, "denyRate") {
		t.Fatalf("%+v", f)
	}
}

func TestBaselineSaveLoadForget(t *testing.T) {
	b := NewBaseline()
	b.Observe("a", steady(0))
	b.Observe("b", steady(0))
	path := filepath.Join(t.TempDir(), "sub", "baseline.json")
	if err := b.Save(path); err != nil {
		t.Fatal(err)
	}
	c := NewBaseline()
	if err := c.Load(path); err != nil || len(c.Keys()) != 2 {
		t.Fatalf("%v %v", c.Keys(), err)
	}
	if err := NewBaseline().Load(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	c.stats["b"].Seen = time.Now().Add(-2 * time.Hour).Unix()
	c.Prune(time.Now().Add(-time.Hour))
	if k := c.Keys(); len(k) != 1 || k[0] != "a" {
		t.Fatal(k)
	}
}
