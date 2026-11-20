// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sample is one tick of edge traffic for one Machine.
type Sample struct {
	Bytes    float64 `json:"bytes"`
	Flows    float64 `json:"flows"`
	Dsts     float64 `json:"dsts"`
	SNIs     float64 `json:"snis"`
	DenyRate float64 `json:"denyRate"`
}

var sampleMetrics = [...]string{"bytes", "flows", "dsts", "snis", "denyRate"}

// minStd keeps a near-constant metric from turning a tiny change into a
// huge z-score: one extra destination on a Machine that always talks to
// exactly one is not an anomaly.
var minStd = [...]float64{4096, 2, 1, 1, 0.05}

func (s Sample) vec() [5]float64 { return [5]float64{s.Bytes, s.Flows, s.Dsts, s.SNIs, s.DenyRate} }

// SampleFrom summarizes one tick's flows and drops.
func SampleFrom(flows []Flow, drops []Drop) Sample {
	var s Sample
	dsts, snis := map[string]struct{}{}, map[string]struct{}{}
	for _, f := range flows {
		s.Bytes += float64(f.Bytes)
		if f.DstIP != "" {
			dsts[f.DstIP] = struct{}{}
		}
		if f.SNI != "" {
			snis[strings.ToLower(f.SNI)] = struct{}{}
		}
	}
	s.Flows, s.Dsts, s.SNIs = float64(len(flows)), float64(len(dsts)), float64(len(snis))
	var denies float64
	for _, d := range drops {
		c := d.Count
		if c <= 0 {
			c = 1
		}
		denies += float64(c)
	}
	if total := s.Flows + denies; total > 0 {
		s.DenyRate = denies / total
	}
	return s
}

type baseStat struct {
	N    int        `json:"n"`
	Mean [5]float64 `json:"mean"`
	Var  [5]float64 `json:"var"`
	Seen int64      `json:"seen"`
}

// Baseline keeps an exponentially weighted mean and variance of each
// Machine's Sample and flags metrics more than K standard deviations
// above it. It is statistics, not a model, and never applies anything.
type Baseline struct {
	Alpha  float64 // EWMA weight of a new sample
	K      float64 // z-score threshold
	Warmup int     // samples before a key can flag

	mu    sync.Mutex
	stats map[string]*baseStat
}

func NewBaseline() *Baseline {
	return &Baseline{Alpha: 0.1, K: 4, Warmup: 30, stats: map[string]*baseStat{}}
}

// BaselineKey groups by tenant so one tenant's traffic never shapes
// another's baseline.
func BaselineKey(tenant, namespace, name string) string {
	return tenant + "|" + namespace + "/" + name
}

// Observe scores s against key's baseline, then folds it in. A deviating
// sample is folded in at a quarter weight so a sustained exfiltration
// does not quickly become the new normal.
func (b *Baseline) Observe(key string, s Sample) []Finding {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stats == nil {
		b.stats = map[string]*baseStat{}
	}
	st := b.stats[key]
	x := s.vec()
	now := time.Now().Unix()
	if st == nil {
		b.stats[key] = &baseStat{N: 1, Mean: x, Seen: now}
		return nil
	}
	st.Seen = now
	var out []Finding
	if st.N >= b.Warmup {
		for i, v := range x {
			std := math.Max(math.Sqrt(st.Var[i]), minStd[i])
			if z := (v - st.Mean[i]) / std; z > b.K {
				out = append(out, Finding{
					Kind:    "baseline_deviation",
					Summary: fmt.Sprintf("%s %.4g is %.1f standard deviations above this Machine's baseline %.4g", sampleMetrics[i], v, z, st.Mean[i]),
				})
			}
		}
	}
	alpha := b.Alpha
	if len(out) > 0 {
		alpha /= 4
	}
	for i, v := range x {
		d := v - st.Mean[i]
		st.Mean[i] += alpha * d
		st.Var[i] = (1 - alpha) * (st.Var[i] + alpha*d*d)
	}
	st.N++
	return out
}

// Prune drops keys not observed since before, so churned Machines do
// not accumulate.
func (b *Baseline) Prune(before time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, st := range b.stats {
		if st.Seen < before.Unix() {
			delete(b.stats, k)
		}
	}
}

func (b *Baseline) Keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.stats))
	for k := range b.stats {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Save writes the state atomically so a restart keeps the warm-up.
func (b *Baseline) Save(path string) error {
	b.mu.Lock()
	raw, err := json.Marshal(b.stats)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load replaces the state from path; a missing file is not an error.
func (b *Baseline) Load(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	stats := map[string]*baseStat{}
	if err := json.Unmarshal(raw, &stats); err != nil {
		return fmt.Errorf("baseline %s: %w", path, err)
	}
	b.mu.Lock()
	b.stats = stats
	b.mu.Unlock()
	return nil
}
