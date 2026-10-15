// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package ebpfedge

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestStableIdentitySurvivesIPChange(t *testing.T) {
	a := StableIdentity("demo", "web")
	b := StableIdentity("demo", "web")
	if a != b {
		t.Fatalf("identity not stable: %d vs %d", a, b)
	}
	if a < IdentityReserved {
		t.Fatalf("identity %d fell in the reserved range", a)
	}
	if StableIdentity("demo", "db") == a {
		t.Fatal("different machines collided")
	}
	spec, err := Compile(EdgeSpec{Namespace: "demo", Machine: "web", AssignedIP: "10.0.0.8"})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := Compile(EdgeSpec{Namespace: "demo", Machine: "web", AssignedIP: "10.0.0.9"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Identity != moved.Identity {
		t.Fatalf("identity changed across IP move: %d -> %d", spec.Identity, moved.Identity)
	}
}

func TestAntiSpoofAndAttributedDrop(t *testing.T) {
	spec, err := Compile(EdgeSpec{
		Namespace:    "demo",
		Machine:      "web",
		AntiSpoof:    true,
		AssignedMAC:  "52:54:00:aa:bb:cc",
		AssignedIP:   "10.0.0.8",
		PolicyName:   "web-tier",
		DefaultAllow: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, dropped := Attribute(spec, PacketMeta{SrcMAC: "52:54:00:11:22:33", SrcIP: "10.0.0.8", DstIP: "1.1.1.1"})
	if !dropped || ev.Reason != ReasonSpoofMAC || ev.PolicyName != "web-tier" {
		t.Fatalf("spoof not attributed: %+v dropped=%v", ev, dropped)
	}
	ev, dropped = Attribute(spec, PacketMeta{SrcMAC: "52:54:00:aa:bb:cc", SrcIP: "10.9.9.9", DstIP: "1.1.1.1"})
	if !dropped || ev.Reason != ReasonSpoofIP {
		t.Fatalf("ip spoof not attributed: %+v", ev)
	}
	if _, dropped := Attribute(spec, PacketMeta{SrcMAC: "52-54-00-AA-BB-CC", SrcIP: "10.0.0.8", DstIP: "1.1.1.1"}); dropped {
		t.Fatal("normalized assigned mac was dropped")
	}
}

func TestSNIAndDNSDeny(t *testing.T) {
	spec, err := Compile(EdgeSpec{
		Namespace:    "demo",
		Machine:      "web",
		DefaultAllow: true,
		AllowSNI:     []string{"*.vendor.com", "api.internal"},
		AllowDNS:     []string{"api.vendor.com"},
		PolicyName:   "egress-443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := Decide(spec, PacketMeta{SNI: "edge.vendor.com"}); d.Reason != ReasonAllow {
		t.Fatalf("suffix SNI should allow, got %s", d.Reason)
	}
	ev, dropped := Attribute(spec, PacketMeta{SNI: "evil.example", DstIP: "9.9.9.9"})
	if !dropped || ev.Reason != ReasonSNIDeny || ev.PolicyName != "egress-443" {
		t.Fatalf("sni deny: %+v", ev)
	}
	ev, dropped = Attribute(spec, PacketMeta{DNSQName: "malware.test."})
	if !dropped || ev.Reason != ReasonDNSDeny {
		t.Fatalf("dns deny: %+v", ev)
	}
	if _, err := Compile(EdgeSpec{Namespace: "demo", Machine: "web", AllowSNI: []string{"http://nope"}}); err == nil {
		t.Fatal("expected invalid SNI")
	}
}

func TestConntrackRestoreIdentityAndBlackhole(t *testing.T) {
	id := StableIdentity("demo", "web")
	now := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	snap, err := ExportConntrack(id, 7, []ConntrackEntry{{
		Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "10.1.0.4", SrcPort: 40000, DstPort: 443, State: "established",
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RestoreConntrack(id, back, now.Add(40*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if res.Restored != 1 || res.BlackholeWindowMs != 40 || res.Identity != id {
		t.Fatalf("restore result: %+v", res)
	}
	if _, err := RestoreConntrack(id+1, back, now); err == nil {
		t.Fatal("identity mismatch must fail closed")
	}
}

func TestLearnIPFromARPDHCPAndND(t *testing.T) {
	arp := make([]byte, 42)
	binary.BigEndian.PutUint16(arp[12:14], 0x0806)
	binary.BigEndian.PutUint16(arp[14+6:14+8], 2)
	copy(arp[14+14:14+18], []byte{10, 0, 0, 8})
	ip, src, ok := LearnIP(arp)
	if !ok || ip != "10.0.0.8" || src != IPSourceARP {
		t.Fatalf("arp learn: %s %s %v", ip, src, ok)
	}

	dhcp := make([]byte, 14+20+8+240)
	binary.BigEndian.PutUint16(dhcp[12:14], 0x0800)
	dhcp[14] = 0x45
	dhcp[14+9] = 17
	binary.BigEndian.PutUint16(dhcp[14+20+2:14+20+4], 68)
	binary.BigEndian.PutUint32(dhcp[14+20+8+236:14+20+8+240], 0x63825363)
	copy(dhcp[14+20+8+16:14+20+8+20], []byte{10, 0, 0, 15})
	ip, src, ok = LearnIP(dhcp)
	if !ok || ip != "10.0.0.15" || src != IPSourceDHCP {
		t.Fatalf("dhcp learn: %s %s %v", ip, src, ok)
	}
}

func TestCaptureBound(t *testing.T) {
	now := time.Now().UTC()
	s, err := NewCapture("demo", "web", "tcp port 443", 15, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Token) != 32 || s.ExpiresAt.Sub(now) != 15*time.Second {
		t.Fatalf("session: %+v", s)
	}
	if _, err := NewCapture("demo", "web", "", 31, now); err == nil {
		t.Fatal("capture over 30s must be rejected")
	}
}

func TestDefaultDenyAndCIDR(t *testing.T) {
	spec, err := Compile(EdgeSpec{
		Namespace:  "demo",
		Machine:    "web",
		AllowCIDRs: []string{"10.0.0.0/8"},
		PolicyName: "east-west",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := Decide(spec, PacketMeta{DstIP: "10.1.2.3"}); d.Action != "allow" {
		t.Fatalf("allow cidr dropped: %+v", d)
	}
	ev, dropped := Attribute(spec, PacketMeta{DstIP: "8.8.8.8", Proto: "tcp", DstPort: 443})
	if !dropped || ev.Reason != ReasonPolicyDeny || ev.PolicyName != "east-west" {
		t.Fatalf("cidr deny: %+v", ev)
	}
}

func TestL7AllowRulesAndXDPFlag(t *testing.T) {
	spec, err := Compile(EdgeSpec{
		Namespace: "demo", Machine: "web", DefaultAllow: true, AntiSpoof: true,
		AssignedIP: "10.0.0.8", DenyCIDRs: []string{"169.254.0.0/16"},
		AllowL7: []L7Rule{{Method: "get", Path: "/api/"}, {Method: "POST", Path: "/api/submit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path string
		allow              bool
	}{
		{"get prefix", "GET", "/api/users", true},
		{"post exact rule", "POST", "/api/submit", true},
		{"post other path", "POST", "/api/users", false},
		{"delete", "DELETE", "/api/users", false},
		{"get outside prefix", "GET", "/admin", false},
	} {
		d := Decide(spec, PacketMeta{DstIP: "10.1.1.1", HTTPMethod: tc.method, HTTPPath: tc.path})
		if (d.Action == "allow") != tc.allow {
			t.Errorf("%s: action %s reason %s", tc.name, d.Action, d.Reason)
		}
		if d.Action == "drop" && (d.Reason != ReasonL7Deny || d.XDP) {
			t.Errorf("%s: reason %s xdp %v", tc.name, d.Reason, d.XDP)
		}
	}
	if d := Decide(spec, PacketMeta{DstIP: "10.1.1.1"}); d.Action != "allow" {
		t.Errorf("non-HTTP packet must skip L7 rules: %+v", d)
	}
	if d := Decide(spec, PacketMeta{SrcIP: "10.0.0.99"}); d.Reason != ReasonSpoofIP || !d.XDP {
		t.Errorf("spoof must be XDP-eligible: %+v", d)
	}
	if d := Decide(spec, PacketMeta{DstIP: "169.254.169.254"}); d.Reason != ReasonPolicyDeny || !d.XDP {
		t.Errorf("deny CIDR must be XDP-eligible: %+v", d)
	}
	if _, err := Compile(EdgeSpec{Namespace: "d", Machine: "m", AllowL7: []L7Rule{{}}}); err == nil {
		t.Error("empty L7 rule must be rejected")
	}
	if _, err := Compile(EdgeSpec{Namespace: "d", Machine: "m", AllowL7: []L7Rule{{Path: "api"}}}); err == nil {
		t.Error("path without leading slash must be rejected")
	}
}

func TestFlowTableAggregatesAndEvicts(t *testing.T) {
	tbl := NewFlowTable(2)
	t0 := time.Unix(1000, 0)
	a := FlowKey{Identity: 300, Proto: "tcp", SrcIP: "10.0.0.8", DstIP: "1.1.1.1", SrcPort: 1, DstPort: 443}
	b := a
	b.DstPort = 80
	c := a
	c.DstPort = 22
	tbl.Record(a, 100, Decision{Action: "allow"}, t0)
	tbl.Record(a, 50, Decision{Action: "drop", Reason: ReasonRateLimit}, t0.Add(time.Second))
	got := tbl.Snapshot()
	if len(got) != 1 || got[0].Bytes != 150 || got[0].Packets != 2 || !got[0].Denied || got[0].Reason != ReasonRateLimit {
		t.Fatalf("aggregate: %+v", got)
	}
	tbl.Record(b, 1, Decision{Action: "allow"}, t0.Add(2*time.Second))
	tbl.Record(c, 1, Decision{Action: "allow"}, t0.Add(3*time.Second))
	got = tbl.Snapshot()
	if len(got) != 2 || got[0].DstPort != 22 || got[1].DstPort != 80 {
		t.Fatalf("eviction must drop the least recently seen flow: %+v", got)
	}
}
