// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ebpfedge

import "testing"

func TestDecodeFlowRecordsCamelAndSnakeAndWrapped(t *testing.T) {
	raw := []byte(`{"flows":[{"proto":"tcp","src_ip":"10.0.0.8","dst_ip":"1.1.1.1","src_port":40000,"dst_port":443,"packets":12,"bytes":8400,"pid":142,"comm":"nginx","scope":"guest","rtt_us":1400,"reason":"allow"}]}`)
	flows, err := DecodeFlowRecords(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("len=%d", len(flows))
	}
	fr := flows[0]
	if fr.SrcIP != "10.0.0.8" || fr.DstPort != 443 || fr.Packets != 12 {
		t.Fatalf("tuple: %+v", fr)
	}
	if fr.Process.PID != 142 || fr.Process.Comm != "nginx" || fr.Process.Scope != AttrScopeGuest {
		t.Fatalf("process: %+v", fr.Process)
	}
	if fr.Latency.RTTUs != 1400 {
		t.Fatalf("rtt: %+v", fr.Latency)
	}
}

func TestDecodeFlowRecordsNestedProcessAndRttMs(t *testing.T) {
	raw := []byte(`[{"identity":300,"proto":"udp","srcIP":"10.0.0.8","dstIP":"9.9.9.9","srcPort":53,"dstPort":53,"packets":2,"process":{"source":"packetwolf","scope":"guest","pid":88,"comm":"coredns","cgroup":"/kubepods/besteffort/pod123"},"latency":{"rttMs":1.5}}]`)
	flows, err := DecodeFlowRecords(raw)
	if err != nil {
		t.Fatal(err)
	}
	fr := flows[0]
	if fr.Process.Source != AttrSourcePacketWolf || fr.Process.Comm != "coredns" {
		t.Fatalf("process: %+v", fr.Process)
	}
	if fr.Latency.RTTUs != 1500 {
		t.Fatalf("rtt ms: %d", fr.Latency.RTTUs)
	}
}

func TestDecodeFlowRecordsHostScopeLandsOnHostProcess(t *testing.T) {
	raw := []byte(`[{"proto":"tcp","srcIP":"10.0.0.8","dstIP":"1.1.1.1","srcPort":1,"dstPort":80,"pid":9,"comm":"vhost","scope":"host"}]`)
	flows, err := DecodeFlowRecords(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !flows[0].Process.IsZero() || flows[0].HostProcess.Comm != "vhost" || flows[0].HostProcess.PID != 9 {
		t.Fatalf("host: process=%+v host=%+v", flows[0].Process, flows[0].HostProcess)
	}
}

func TestDecodeDropEventsPolicyCountAliases(t *testing.T) {
	raw := []byte(`{"drops":[{"reason":"sni_deny","policy":"egress-443","src_ip":"10.0.0.8","dst_ip":"9.9.9.9","proto":"tcp","dst_port":443,"count":4,"process":{"comm":"curl","pid":77,"scope":"guest"}}]}`)
	drops, err := DecodeDropEvents(raw)
	if err != nil {
		t.Fatal(err)
	}
	ev := drops[0]
	if ev.Reason != ReasonSNIDeny || ev.PolicyName != "egress-443" || ev.Packets != 4 {
		t.Fatalf("drop aliases: %+v", ev)
	}
	if ev.Process.Comm != "curl" || ev.Process.PID != 77 {
		t.Fatalf("drop process: %+v", ev.Process)
	}
}

func TestDecodeDropDoesNotTreatMachineNameAsComm(t *testing.T) {
	raw := []byte(`{"machine":"web","namespace":"demo","name":"web","reason":"spoof_ip","srcIP":"10.9.9.9"}`)
	drops, err := DecodeDropEvents(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !drops[0].Process.IsZero() {
		t.Fatalf("machine name leaked into process: %+v", drops[0].Process)
	}
	if drops[0].Machine != "web" || drops[0].Reason != ReasonSpoofIP {
		t.Fatalf("drop: %+v", drops[0])
	}
}

func TestDecodeDataplaneStatsLatency(t *testing.T) {
	raw := []byte(`{"rx_packets":10,"txPackets":12,"rxBytes":100,"tx_bytes":200,"dropped":3,"flows":8,"rttMs":2.5,"min_rtt_us":800}`)
	s, err := DecodeDataplaneStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.RxPackets != 10 || s.TxPackets != 12 || s.RxBytes != 100 || s.TxBytes != 200 || s.Dropped != 3 || s.Flows != 8 {
		t.Fatalf("counters: %+v", s)
	}
	if s.Latency.RTTUs != 2500 || s.Latency.MinRTTUs != 800 {
		t.Fatalf("latency: %+v", s.Latency)
	}
}

func TestDecodeRejectsNonJSON(t *testing.T) {
	if _, err := DecodeFlowRecords([]byte("not-json")); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DecodeDropEvents(nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DecodeDataplaneStats([]byte("[]")); err == nil {
		t.Fatal("stats must be an object")
	}
}

func TestDecodeDataplaneStatsPercentilesAndSamples(t *testing.T) {
	s, err := DecodeDataplaneStats([]byte(`{"rxPackets":1,"avg_rtt_us":1400,"p50Us":1200,"p95_us":2500,"minRttUs":800,"maxRttUs":3000,"samples":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Latency.AvgRTTUs != 1400 || s.Latency.P50Us != 1200 || s.Latency.P95Us != 2500 || s.Latency.Samples != 12 {
		t.Fatalf("explicit percentiles: %+v", s.Latency)
	}
	// A sample array fills gaps; explicit fields win.
	s, err = DecodeDataplaneStats([]byte(`{"p50Us":1111,"rtts":[800,1000,1200,1400,5000]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Latency.P50Us != 1111 {
		t.Fatalf("explicit p50 should win: %d", s.Latency.P50Us)
	}
	if s.Latency.MinRTTUs != 800 || s.Latency.MaxRTTUs != 5000 || s.Latency.Samples != 5 || s.Latency.AvgRTTUs != 1880 {
		t.Fatalf("computed gaps: %+v", s.Latency)
	}
}

func TestDecodeDataplaneStatsNestedSampleArray(t *testing.T) {
	s, err := DecodeDataplaneStats([]byte(`{"latency":{"rtt_us_samples":[100,200,300]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Latency.Samples != 3 || s.Latency.MinRTTUs != 100 || s.Latency.MaxRTTUs != 300 || s.Latency.AvgRTTUs != 200 {
		t.Fatalf("nested samples: %+v", s.Latency)
	}
}
