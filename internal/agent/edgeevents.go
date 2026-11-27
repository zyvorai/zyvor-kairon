// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/model"
)

// edgeEventWindow suppresses an identical Warning for the same Machine,
// so a steady beacon or deny loop does not write one Event per tick.
const edgeEventWindow = 10 * time.Minute

const edgeFlowLimit = 256

// baselineSaveEvery bounds how much warm-up a crash loses; baselineIdle
// drops Machines that stopped reporting (deleted or moved away).
const (
	baselineSaveEvery = 5 * time.Minute
	baselineIdle      = time.Hour
)

type edgeBaselineState struct {
	once  sync.Once
	b     *agentplane.Baseline
	mu    sync.Mutex
	saved time.Time
}

// baseline returns the node's per-Machine traffic baseline, loading the
// snapshot at EdgeBaselinePath on first use.
func (a *Agent) baseline() *agentplane.Baseline {
	s := &a.edgeBaseline
	s.once.Do(func() {
		s.b = agentplane.NewBaseline()
		if a.EdgeBaselinePath == "" {
			return
		}
		if err := s.b.Load(a.EdgeBaselinePath); err != nil {
			a.log().Warn("edge baseline load failed; starting cold", "path", a.EdgeBaselinePath, "error", err)
		}
		s.saved = time.Now()
	})
	return s.b
}

func (a *Agent) saveBaseline(b *agentplane.Baseline) {
	s := &a.edgeBaseline
	if a.EdgeBaselinePath == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.saved) < baselineSaveEvery {
		return
	}
	s.saved = now
	b.Prune(now.Add(-baselineIdle))
	if err := b.Save(a.EdgeBaselinePath); err != nil {
		a.log().Warn("edge baseline save failed", "path", a.EdgeBaselinePath, "error", err)
	}
}

type edgeEventDedup struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether key may emit now, and records it when it may.
func (d *edgeEventDedup) allow(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]time.Time{}
	}
	if t, ok := d.last[key]; ok && now.Sub(t) < edgeEventWindow {
		return false
	}
	for k, t := range d.last {
		if now.Sub(t) >= edgeEventWindow {
			delete(d.last, k)
		}
	}
	d.last[key] = now
	return true
}

// edgeFlows reads FluxVM's recent flows for one runtime. FluxVM returns
// untyped JSON, so the common field spellings are accepted. When the
// dump includes latency or process attribution, it is also fed into
// kairon-node edge metrics.
func (a *Agent) edgeFlows(ctx context.Context, m model.Machine, runtimeID string) []agentplane.Flow {
	if a.Flux == nil || runtimeID == "" {
		return nil
	}
	raw, err := a.Flux.GetVMNetworkFlows(ctx, runtimeID, edgeFlowLimit)
	if err != nil {
		return nil
	}
	a.observeFlowMetrics(m, raw)
	return parseEdgeFlows(raw)
}

// observeFlowMetrics decodes a FluxVM flow dump and records RTT and
// attribution-source metrics. Best effort: a decode miss is ignored.
func (a *Agent) observeFlowMetrics(m model.Machine, raw []byte) {
	if a.Metrics == nil || len(raw) == 0 {
		return
	}
	rec := a.Metrics.Edge()
	if rec == nil {
		return
	}
	flows, err := ebpfedge.DecodeFlowRecords(raw)
	if err != nil {
		return
	}
	rec.ObserveFlows(m.Namespace(), m.Metadata.Name, flows)
}

func parseEdgeFlows(raw json.RawMessage) []agentplane.Flow {
	var body struct {
		Items []struct {
			DstIP       string `json:"dstIP"`
			DstIPSnake  string `json:"dst_ip"`
			Destination string `json:"destination"`
			SNI         string `json:"sni"`
			DNS         string `json:"dns"`
			QName       string `json:"qname"`
			Bytes       int    `json:"bytes"`
			IntervalSec int    `json:"intervalSec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	out := make([]agentplane.Flow, 0, len(body.Items))
	for _, it := range body.Items {
		f := agentplane.Flow{DstIP: it.DstIP, SNI: it.SNI, DNS: it.DNS, Bytes: it.Bytes, IntervalSec: it.IntervalSec}
		if f.DstIP == "" {
			f.DstIP = it.DstIPSnake
		}
		if f.DstIP == "" {
			f.DstIP = it.Destination
		}
		if f.DNS == "" {
			f.DNS = it.QName
		}
		out = append(out, f)
	}
	return out
}

// emitEdgeEvents turns this tick's drops and flows into Warning Events on
// the Machine. Best effort: a failed write is logged and never blocks the
// network reconcile.
func (a *Agent) emitEdgeEvents(ctx context.Context, m model.Machine, drops []agentplane.Drop, flows []agentplane.Flow) {
	if a.Kube == nil {
		return
	}
	findings := agentplane.Detect(flows, drops)
	if b := a.baseline(); b != nil {
		key := agentplane.BaselineKey(m.Metadata.Labels[agentplane.LabelTenant], m.Namespace(), m.Metadata.Name)
		findings = append(findings, b.Observe(key, agentplane.SampleFrom(flows, drops))...)
		a.saveBaseline(b)
	}
	events := agentplane.EventsFromDrops(m.Metadata.Name, drops)
	events = append(events, agentplane.EventsFromFindings(m.Metadata.Name, findings)...)
	now := time.Now()
	for _, ev := range events {
		key := m.Namespace() + "/" + m.Metadata.Name + "|" + ev.Reason + "|" + ev.Message
		if !a.edgeEvents.allow(key, now) {
			continue
		}
		if err := a.Kube.RecordEvent(ctx, m.Namespace(), "Machine", m.Metadata.Name, m.Metadata.UID, ev.Reason, ev.Message, ev.Type); err != nil {
			a.log().Warn("edge event record failed", "machine", m.Metadata.Name, "reason", ev.Reason, "error", err)
		}
	}
}
