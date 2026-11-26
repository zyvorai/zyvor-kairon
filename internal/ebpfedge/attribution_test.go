// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package ebpfedge

import (
	"context"
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
