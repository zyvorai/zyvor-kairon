// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ebpfedge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestProcessAttrDisplay(t *testing.T) {
	if (ProcessAttr{}).Display() != "-" {
		t.Fatal("zero should display as -")
	}
	got := ProcessAttr{Comm: "nginx", PID: 142, Scope: AttrScopeGuest}.Display()
	if got != "guest:nginx[pid=142]" {
		t.Fatalf("display: %q", got)
	}
	got = ProcessAttr{Cgroup: "/sys/fs/cgroup/machine.slice/vm-web"}.Display()
	if got != "cgroup=/sys/fs/cgroup/machine.slice/vm-web" {
		t.Fatalf("cgroup-only: %q", got)
	}
}

func TestFormatMicroseconds(t *testing.T) {
	cases := map[uint64]string{
		0:         "-",
		800:       "800µs",
		1000:      "1ms",
		1400:      "1.4ms",
		1_000_000: "1s",
		2_500_000: "2.50s",
	}
	for us, want := range cases {
		if got := FormatMicroseconds(us); got != want {
			t.Errorf("%d: got %q want %q", us, got, want)
		}
	}
}

func TestAttributionMergePrefersExistingAndFillsGaps(t *testing.T) {
	guest := Attribution{Process: ProcessAttr{Source: AttrSourcePacketWolf, Scope: AttrScopeGuest, Comm: "curl", PID: 88}}
	host := Attribution{HostProcess: ProcessAttr{Source: AttrSourceFluxVM, Scope: AttrScopeHost, Comm: "vhost", PID: 9}, Latency: LatencyAttr{RTTUs: 1400}}
	merged := guest.Merge(host)
	if merged.Process.PID != 88 || merged.Process.Comm != "curl" {
		t.Fatalf("guest overwritten: %+v", merged.Process)
	}
	if merged.HostProcess.PID != 9 || merged.Latency.RTTUs != 1400 {
		t.Fatalf("host/latency not filled: %+v", merged)
	}
	// Existing RTT must not be replaced.
	left := Attribution{Latency: LatencyAttr{RTTUs: 500}}
	if left.Merge(host).Latency.RTTUs != 500 {
		t.Fatal("existing RTT was overwritten")
	}
}

func TestNormalizeSplitsHostScopedProcess(t *testing.T) {
	a := Attribution{Process: ProcessAttr{Scope: AttrScopeHost, PID: 3, Comm: "qemu-system"}}.Normalize()
	if !a.Process.IsZero() || a.HostProcess.PID != 3 {
		t.Fatalf("host process not split: %+v", a)
	}
}

func TestChainAttributorMergesSources(t *testing.T) {
	key := FlowKey{Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 9, DstPort: 443}
	flux := MapAttributor{
		Source: AttrSourceFluxVM,
		Flows: map[FlowKey]Attribution{
			key: {HostProcess: ProcessAttr{Source: AttrSourceFluxVM, Scope: AttrScopeHost, Comm: "vhost", PID: 7}, Latency: LatencyAttr{RTTUs: 2100}},
		},
	}
	wolf := MapAttributor{
		Source: AttrSourcePacketWolf,
		Flows: map[FlowKey]Attribution{
			key: {Process: ProcessAttr{Source: AttrSourcePacketWolf, Scope: AttrScopeGuest, Comm: "nginx", PID: 142}},
		},
	}
	got, err := (Chain{flux, wolf}).Lookup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Process.Comm != "nginx" || got.HostProcess.Comm != "vhost" || got.Latency.RTTUs != 2100 {
		t.Fatalf("chain merge: %+v", got)
	}
	if (Chain{flux, wolf}).Name() != "fluxvm+packetwolf" {
		t.Fatalf("name: %s", (Chain{flux, wolf}).Name())
	}
}

func TestAnnotateFlowsAndDropsFillZerosOnly(t *testing.T) {
	key := FlowKey{Identity: 300, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 1, DstPort: 443}
	attr := MapAttributor{Source: AttrSourceFluxVM, Flows: map[FlowKey]Attribution{
		key: {
			Process: ProcessAttr{Comm: "nginx", PID: 142, Source: AttrSourceFluxVM, Scope: AttrScopeGuest},
			Latency: LatencyAttr{RTTUs: 1500},
		},
	}}
	flows := []FlowRecord{{FlowKey: key, Packets: 3, Process: ProcessAttr{Comm: "already"}}}
	if err := AnnotateFlows(context.Background(), flows, attr); err != nil {
		t.Fatal(err)
	}
	if flows[0].Process.Comm != "already" || flows[0].Process.PID != 142 || flows[0].Latency.RTTUs != 1500 {
		t.Fatalf("annotate flow: %+v", flows[0])
	}
	drops := []DropEvent{{SrcIP: key.SrcIP, DstIP: key.DstIP, Proto: key.Proto, SrcPort: key.SrcPort, DstPort: key.DstPort, Identity: key.Identity, Reason: ReasonPolicyDeny}}
	if err := AnnotateDrops(context.Background(), drops, attr); err != nil {
		t.Fatal(err)
	}
	if drops[0].Process.Comm != "nginx" || drops[0].Latency.RTTUs != 1500 {
		t.Fatalf("annotate drop: %+v", drops[0])
	}
}

func TestFlowTableRecordAttrKeepsAttribution(t *testing.T) {
	tbl := NewFlowTable(4)
	key := FlowKey{Identity: 300, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 1, DstPort: 443}
	tbl.RecordAttr(key, 100, Decision{Action: "allow"}, Attribution{
		Process: ProcessAttr{Comm: "curl", PID: 88, Scope: AttrScopeGuest},
		Latency: LatencyAttr{RTTUs: 800},
	}, time.Unix(1000, 0))
	got := tbl.Snapshot()
	if len(got) != 1 || got[0].Process.Comm != "curl" || got[0].Latency.RTTUs != 800 {
		t.Fatalf("record attr: %+v", got)
	}
}

func TestAttributeCopiesProcessFromPacket(t *testing.T) {
	spec, err := Compile(EdgeSpec{
		Namespace: "demo", Machine: "web", AntiSpoof: true,
		AssignedMAC: "52:54:00:aa:bb:cc", AssignedIP: "10.0.0.8", PolicyName: "web-tier", DefaultAllow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, dropped := Attribute(spec, PacketMeta{
		SrcMAC: "52:54:00:11:22:33", SrcIP: "10.0.0.8", DstIP: "1.1.1.1",
		Proto: "tcp", SrcPort: 40000, DstPort: 443,
		Process: ProcessAttr{Comm: "scraper", PID: 9, Scope: AttrScopeGuest, Source: AttrSourcePacketWolf},
	})
	if !dropped || ev.Process.Comm != "scraper" || ev.SrcPort != 40000 || ev.Identity == 0 {
		t.Fatalf("attributed drop missing process: %+v dropped=%v", ev, dropped)
	}
}

func TestChainSkipsNilAndEmpty(t *testing.T) {
	key := FlowKey{Proto: "tcp", SrcIP: "10.0.0.1", DstIP: "10.0.0.2", DstPort: 80}
	filled := MapAttributor{
		Source: AttrSourceFluxVM,
		Flows:  map[FlowKey]Attribution{key: {Process: ProcessAttr{Comm: "nginx", PID: 1, Scope: AttrScopeGuest}}},
	}
	var empty Chain
	got, err := empty.Lookup(context.Background(), key)
	if err != nil || !got.IsZero() {
		t.Fatalf("empty chain: %+v err=%v", got, err)
	}
	if empty.Name() != "" {
		t.Fatalf("empty name: %q", empty.Name())
	}
	ch := Chain{nil, MapAttributor{Source: "noop"}, filled, nil}
	got, err = ch.Lookup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Process.Comm != "nginx" || got.Process.PID != 1 {
		t.Fatalf("nil members should be skipped: %+v", got)
	}
	if ch.Name() != "noop+fluxvm" {
		t.Fatalf("name should skip nils: %q", ch.Name())
	}
}

func TestChainOrderingFirstWinsOverlappingFields(t *testing.T) {
	key := FlowKey{Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 9, DstPort: 443}
	first := MapAttributor{Source: "first", Flows: map[FlowKey]Attribution{
		key: {Process: ProcessAttr{Comm: "nginx", PID: 1, Scope: AttrScopeGuest}, Latency: LatencyAttr{RTTUs: 500}},
	}}
	second := MapAttributor{Source: "second", Flows: map[FlowKey]Attribution{
		key: {Process: ProcessAttr{Comm: "curl", PID: 99, Scope: AttrScopeGuest, Cgroup: "/guest/curl"}, Latency: LatencyAttr{RTTUs: 9000, MinRTTUs: 200}},
	}}
	got, err := (Chain{first, second}).Lookup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Process.Comm != "nginx" || got.Process.PID != 1 {
		t.Fatalf("later attributor overwrote: %+v", got.Process)
	}
	if got.Process.Cgroup != "/guest/curl" {
		t.Fatalf("gap cgroup not filled: %+v", got.Process)
	}
	if got.Latency.RTTUs != 500 || got.Latency.MinRTTUs != 200 {
		t.Fatalf("latency merge: %+v", got.Latency)
	}
	// Reversing the chain must prefer the other comm.
	rev, err := (Chain{second, first}).Lookup(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Process.Comm != "curl" || rev.Process.PID != 99 || rev.Latency.RTTUs != 9000 {
		t.Fatalf("reversed order: %+v", rev)
	}
}

func TestChainLookupErrorAborts(t *testing.T) {
	key := FlowKey{Proto: "udp", DstPort: 53}
	boom := errors.New("lookup failed")
	first := MapAttributor{Source: "ok", Flows: map[FlowKey]Attribution{
		key: {Process: ProcessAttr{Comm: "coredns", PID: 8}},
	}}
	ch := Chain{first, errAttributor{err: boom}}
	got, err := ch.Lookup(context.Background(), key)
	if !errors.Is(err, boom) {
		t.Fatalf("want lookup failed, got err=%v", err)
	}
	if got.Process.Comm != "coredns" {
		t.Fatalf("partial merge should still be returned: %+v", got)
	}
}

type errAttributor struct{ err error }

func (e errAttributor) Name() string { return "err" }
func (e errAttributor) Lookup(context.Context, FlowKey) (Attribution, error) {
	return Attribution{}, e.err
}

func TestMapAttributorMissAndNilMap(t *testing.T) {
	key := FlowKey{Proto: "tcp", DstPort: 22}
	var empty MapAttributor
	got, err := empty.Lookup(context.Background(), key)
	if err != nil || !got.IsZero() {
		t.Fatalf("nil map: %+v err=%v", got, err)
	}
	if empty.Name() != "map" {
		t.Fatalf("default name: %q", empty.Name())
	}
	attr := MapAttributor{Source: AttrSourcePacketWolf, Flows: map[FlowKey]Attribution{
		FlowKey{Proto: "tcp", DstPort: 80}: {Process: ProcessAttr{Comm: "nginx"}},
	}}
	got, err = attr.Lookup(context.Background(), key)
	if err != nil || !got.IsZero() {
		t.Fatalf("miss should be zero, not an error: %+v err=%v", got, err)
	}
}

func TestAnnotateNilAttributorAndMiss(t *testing.T) {
	key := FlowKey{Identity: 7, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 2, DstPort: 443}
	flows := []FlowRecord{{FlowKey: key, Packets: 4, Process: ProcessAttr{Comm: "keep"}}}
	if err := AnnotateFlows(context.Background(), flows, nil); err != nil {
		t.Fatal(err)
	}
	if flows[0].Process.Comm != "keep" || !flows[0].Latency.IsZero() {
		t.Fatalf("nil attributor mutated flow: %+v", flows[0])
	}
	drops := []DropEvent{{Reason: ReasonPolicyDeny, SrcIP: key.SrcIP}}
	if err := AnnotateDrops(context.Background(), drops, nil); err != nil {
		t.Fatal(err)
	}
	if !drops[0].Process.IsZero() {
		t.Fatalf("nil attributor mutated drop: %+v", drops[0])
	}
	miss := MapAttributor{Source: AttrSourceFluxVM, Flows: map[FlowKey]Attribution{}}
	if err := AnnotateFlows(context.Background(), flows, miss); err != nil {
		t.Fatal(err)
	}
	if flows[0].Process.Comm != "keep" || flows[0].Process.PID != 0 {
		t.Fatalf("miss should leave the record: %+v", flows[0])
	}
	other := FlowKey{Identity: 7, Proto: "tcp", SrcIP: "10.0.0.9", DstIP: "1.1.1.1", SrcPort: 3, DstPort: 80}
	attr := MapAttributor{Flows: map[FlowKey]Attribution{
		other: {Process: ProcessAttr{Comm: "sshd", PID: 15, Scope: AttrScopeGuest}},
	}}
	mixed := []FlowRecord{{FlowKey: key}, {FlowKey: other}}
	if err := AnnotateFlows(context.Background(), mixed, attr); err != nil {
		t.Fatal(err)
	}
	if !mixed[0].Process.IsZero() || mixed[1].Process.Comm != "sshd" {
		t.Fatalf("hit/miss mix: %+v", mixed)
	}
}

func TestAnnotateDropsLookupUsesTuple(t *testing.T) {
	key := FlowKey{Identity: 11, Proto: "tcp", SrcIP: "10.1.0.2", DstIP: "9.9.9.9", SrcPort: 50000, DstPort: 443}
	attr := MapAttributor{Flows: map[FlowKey]Attribution{
		key: {HostProcess: ProcessAttr{Comm: "vhost", PID: 4, Scope: AttrScopeHost, Source: AttrSourceFluxVM}},
	}}
	drops := []DropEvent{{
		Reason: ReasonSNIDeny, Identity: key.Identity, Proto: key.Proto,
		SrcIP: key.SrcIP, DstIP: key.DstIP, SrcPort: key.SrcPort, DstPort: key.DstPort,
	}}
	if err := AnnotateDrops(context.Background(), drops, attr); err != nil {
		t.Fatal(err)
	}
	if drops[0].HostProcess.Comm != "vhost" || !drops[0].Process.IsZero() {
		t.Fatalf("host scope should stay on HostProcess: %+v", drops[0])
	}
}
func TestSummarizeRTT(t *testing.T) {
	if !SummarizeRTT(nil).IsZero() {
		t.Fatal("empty samples should be zero")
	}
	got := SummarizeRTT([]uint64{800, 1000, 1200, 1400, 5000})
	if got.Samples != 5 || got.MinRTTUs != 800 || got.MaxRTTUs != 5000 {
		t.Fatalf("bounds: %+v", got)
	}
	if got.AvgRTTUs != 1880 || got.P50Us != 1200 || got.P95Us != 5000 {
		t.Fatalf("avg/percentiles: %+v", got)
	}
	// Input must not be reordered.
	in := []uint64{5000, 800}
	_ = SummarizeRTT(in)
	if in[0] != 5000 || in[1] != 800 {
		t.Fatalf("input mutated: %v", in)
	}
}
