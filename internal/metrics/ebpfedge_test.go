// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

func TestEdgeRecorderDropAndRestore(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	r.ObserveDrop("demo", "web", "sni_deny", "egress-443")
	r.ObserveDrop("demo", "web", "spoof_mac", "")
	r.ObserveConntrackRestore("demo", "web", 4, 40)
	if got := testutil.ToFloat64(r.Drops.WithLabelValues("demo", "web", "sni_deny", "egress-443")); got != 1 {
		t.Fatalf("sni drops = %v", got)
	}
	if got := testutil.ToFloat64(r.Drops.WithLabelValues("demo", "web", "spoof_mac", "-")); got != 1 {
		t.Fatalf("spoof drops = %v", got)
	}
	if got := testutil.ToFloat64(r.ConntrackRestored); got != 4 {
		t.Fatalf("restored = %v", got)
	}
	if got := testutil.ToFloat64(r.BlackholeWindow.WithLabelValues("demo", "web")); got != 40 {
		t.Fatalf("blackhole window = %v, want 40", got)
	}
}

func TestSetBlackholeWindowIgnoresInvalid(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	r.SetBlackholeWindow("", "web", 10)
	r.SetBlackholeWindow("demo", "", 10)
	r.SetBlackholeWindow("demo", "web", -1)
	if got := testutil.CollectAndCount(r.BlackholeWindow); got != 0 {
		t.Fatalf("expected no series, got %d", got)
	}
	r.SetBlackholeWindow("demo", "web", 0)
	if got := testutil.ToFloat64(r.BlackholeWindow.WithLabelValues("demo", "web")); got != 0 {
		t.Fatalf("zero window should be recorded: %v", got)
	}
}

func TestObserveFlowsRTTAndAttribution(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	key := ebpfedge.FlowRecord{
		FlowKey: ebpfedge.FlowKey{Identity: 1, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 40000, DstPort: 443},
		Latency: ebpfedge.LatencyAttr{RTTUs: 1400},
		Process: ebpfedge.ProcessAttr{Source: ebpfedge.AttrSourcePacketWolf, Scope: ebpfedge.AttrScopeGuest, Comm: "nginx", PID: 142},
	}
	plain := ebpfedge.FlowRecord{
		FlowKey: ebpfedge.FlowKey{Identity: 1, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "9.9.9.9", SrcPort: 53, DstPort: 53},
	}
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{key, plain})
	// Same dump again must not re-count the same RTT or first-seen flow.
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{key, plain})
	if c, s := histogramCountSum(t, r.FlowRTT, "demo", "web"); c != 1 || s != 0.0014 {
		t.Fatalf("rtt histogram count=%d sum=%v, want 1 / 0.0014s", c, s)
	}
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "packetwolf")); got != 1 {
		t.Fatalf("packetwolf attributed = %v", got)
	}
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "none")); got != 1 {
		t.Fatalf("unattributed = %v", got)
	}
	// A changed RTT is a new histogram observation; attribution stays once.
	key.Latency.RTTUs = 2000
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{key})
	if c, s := histogramCountSum(t, r.FlowRTT, "demo", "web"); c != 2 || s < 0.0033 || s > 0.0035 {
		t.Fatalf("updated rtt count=%d sum=%v", c, s)
	}
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "packetwolf")); got != 1 {
		t.Fatalf("attribution re-counted: %v", got)
	}
}

func TestObserveFlowsNoProcessLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{{
		FlowKey: ebpfedge.FlowKey{Proto: "tcp", DstPort: 80},
		Process: ebpfedge.ProcessAttr{Source: ebpfedge.AttrSourceFluxVM, Comm: "nginx", PID: 9},
		Latency: ebpfedge.LatencyAttr{AvgRTTUs: 800},
	}})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		name := mf.GetName()
		if name != "kairon_net_flow_rtt_seconds" && name != "kairon_net_flows_attributed_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "pid", "comm", "process", "cgroup":
					t.Fatalf("%s has process label %s=%s", name, l.GetName(), l.GetValue())
				}
			}
		}
	}
	if c, s := histogramCountSum(t, r.FlowRTT, "demo", "web"); c != 1 || s != 0.0008 {
		t.Fatalf("avg rtt used as sample: count=%d sum=%v", c, s)
	}
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "fluxvm")); got != 1 {
		t.Fatalf("fluxvm source = %v", got)
	}
}

func histogramCountSum(t *testing.T, vec *prometheus.HistogramVec, namespace, machine string) (uint64, float64) {
	t.Helper()
	obs, err := vec.GetMetricWithLabelValues(namespace, machine)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("not a histogram: %T", obs)
	}
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestObserveFlowsUnknownSourceAndEviction(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewEdgeRecorder(reg)
	flow := ebpfedge.FlowRecord{
		FlowKey: ebpfedge.FlowKey{Identity: 1, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 1, DstPort: 80},
		Process: ebpfedge.ProcessAttr{Source: "custom-tool", Comm: "x", PID: 1},
		Latency: ebpfedge.LatencyAttr{RTTUs: 1000},
	}
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{flow})
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "unknown")); got != 1 {
		t.Fatalf("custom source must map to unknown, got %v", got)
	}
	if got := testutil.CollectAndCount(r.FlowsAttributed); got != 1 {
		t.Fatalf("unexpected source series: %d", got)
	}
	// Gone from the dump: maps evict, so the same key is counted again
	// when it returns (same posture as drop-counter resets).
	r.ObserveFlows("demo", "web", nil)
	r.ObserveFlows("demo", "web", []ebpfedge.FlowRecord{flow})
	if got := testutil.ToFloat64(r.FlowsAttributed.WithLabelValues("demo", "web", "unknown")); got != 2 {
		t.Fatalf("after eviction recount = %v", got)
	}
	if c, _ := histogramCountSum(t, r.FlowRTT, "demo", "web"); c != 2 {
		t.Fatalf("rtt re-observed after eviction: count=%d", c)
	}
}
