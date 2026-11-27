// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/ebpfedge"
)

func TestFormatFlowTableIncludesProcessAndRTT(t *testing.T) {
	var buf bytes.Buffer
	formatFlowTable(&buf, []ebpfedge.FlowRecord{{
		FlowKey: ebpfedge.FlowKey{Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 40000, DstPort: 443},
		Packets: 12, Bytes: 8400,
		Process: ebpfedge.ProcessAttr{Comm: "nginx", PID: 142, Scope: ebpfedge.AttrScopeGuest},
		Latency: ebpfedge.LatencyAttr{RTTUs: 1400},
	}})
	got := buf.String()
	for _, want := range []string{"PROTO", "RTT", "PROCESS", "tcp", "10.0.0.8:40000", "1.1.1.1:443", "1.4ms", "guest:nginx[pid=142]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\tHOST\t") || strings.Contains(got, " HOST ") {
		t.Fatalf("empty host column should be omitted:\n%s", got)
	}
}

func TestFormatFlowTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	formatFlowTable(&buf, nil)
	if strings.TrimSpace(buf.String()) != "no flows" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestFormatDropTableProcessColumn(t *testing.T) {
	var buf bytes.Buffer
	formatDropTable(&buf, []ebpfedge.DropEvent{{
		Reason: ebpfedge.ReasonSNIDeny, PolicyName: "egress-443",
		SrcIP: "10.0.0.8", DstIP: "9.9.9.9", DstPort: 443, Packets: 4,
		Process: ebpfedge.ProcessAttr{Comm: "curl", PID: 77, Scope: ebpfedge.AttrScopeGuest},
	}})
	got := buf.String()
	for _, want := range []string{"REASON", "PROCESS", "sni_deny", "egress-443", "guest:curl[pid=77]", "4"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatDataplaneStatsShowsRTT(t *testing.T) {
	var buf bytes.Buffer
	formatDataplaneStats(&buf, ebpfedge.DataplaneStats{
		RxPackets: 10, TxPackets: 12, RxBytes: 100, TxBytes: 200, Dropped: 3, Flows: 8,
		Latency: ebpfedge.LatencyAttr{RTTUs: 2500, MinRTTUs: 800},
	})
	got := buf.String()
	for _, want := range []string{"rx packets", "10", "dropped", "3", "flows", "8", "rtt", "min 800µs", "avg 2.5ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "p50") || strings.Contains(got, "samples") {
		t.Fatalf("absent percentiles should be hidden:\n%s", got)
	}
}

func TestFormatDataplaneStatsRTTSummaryHiddenWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	formatDataplaneStats(&buf, ebpfedge.DataplaneStats{RxPackets: 1, TxPackets: 2})
	got := buf.String()
	if strings.Contains(got, "rtt") || strings.Contains(got, "p50") || strings.Contains(got, "samples") {
		t.Fatalf("rtt summary should be hidden:\n%s", got)
	}
}

func TestFormatDataplaneStatsRTTSummaryFull(t *testing.T) {
	var buf bytes.Buffer
	formatDataplaneStats(&buf, ebpfedge.DataplaneStats{
		RxPackets: 1,
		Latency: ebpfedge.LatencyAttr{
			Samples: 12, MinRTTUs: 800, AvgRTTUs: 1400, P50Us: 1200, P95Us: 2500, MaxRTTUs: 3000,
		},
	})
	got := buf.String()
	for _, want := range []string{"rtt", "samples 12", "min 800µs", "avg 1.4ms", "p50 1.2ms", "p95 2.5ms", "max 3ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPrintFormattedObservabilityStatsFromSampleArray(t *testing.T) {
	var buf bytes.Buffer
	ok := printFormattedObservability(&buf, "network-stats", []byte(`{"rxPackets":1,"rtts":[800,1000,1200,1400,5000]}`))
	if !ok {
		t.Fatal("expected stats formatting")
	}
	got := buf.String()
	for _, want := range []string{"samples 5", "min 800µs", "avg 1.9ms", "p50 1.2ms", "p95 5ms", "max 5ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPrintFormattedObservability(t *testing.T) {
	var buf bytes.Buffer
	ok := printFormattedObservability(&buf, "network-flows", []byte(`{"flows":[{"proto":"tcp","srcIP":"10.0.0.8","dstIP":"1.1.1.1","srcPort":1,"dstPort":80,"packets":2,"rttUs":900}]}`))
	if !ok || !strings.Contains(buf.String(), "900µs") {
		t.Fatalf("flows table: ok=%v\n%s", ok, buf.String())
	}
	buf.Reset()
	if printFormattedObservability(&buf, "network-flows", []byte(`{"message":"dataplane not attached"}`)) {
		t.Fatalf("unrelated object should fall back, got:\n%s", buf.String())
	}
	buf.Reset()
	ok = printFormattedObservability(&buf, "network-stats", []byte(`{"rxPackets":1,"rtt_us":500}`))
	if !ok || !strings.Contains(buf.String(), "500µs") {
		t.Fatalf("stats: ok=%v\n%s", ok, buf.String())
	}
	if printFormattedObservability(&buf, "network-effective", []byte(`{"allowCidrs":["10.0.0.0/8"]}`)) {
		t.Fatal("effective policy should stay JSON")
	}
}
