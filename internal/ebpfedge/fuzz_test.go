// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package ebpfedge

import (
	"encoding/json"
	"testing"
	"time"
)

func FuzzUnmarshalSnapshot(f *testing.F) {
	f.Add([]byte(`{"identity":300,"generation":1,"exportedAt":"2026-10-14T12:00:00Z","entries":[{"proto":"tcp","srcIP":"10.0.0.8","dstIP":"10.1.0.4","srcPort":1,"dstPort":443,"state":"established"}]}`))
	f.Add([]byte(`{"entries":[{"proto":"icmp","srcIP":"::1","dstIP":"fe80::1"}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		snap, err := UnmarshalSnapshot(json.RawMessage(raw))
		if err != nil {
			return
		}
		_, _ = RestoreConntrack(snap.Identity, snap, time.Now().UTC())
	})
}

func FuzzCompileAndDecide(f *testing.F) {
	f.Add("demo", "web", "10.0.0.0/8", "*.example.com", "443", "10.1.2.3", "api.example.com")
	f.Add("", "", "not-a-cidr", "*", "70000", "", "")
	f.Fuzz(func(t *testing.T, ns, machine, cidr, name, port, dstIP, qname string) {
		spec, err := Compile(EdgeSpec{
			Namespace: ns, Machine: machine,
			AllowCIDRs: []string{cidr}, AllowDNS: []string{name}, AllowSNI: []string{name},
			AllowPorts: []string{port}, AllowL7: []L7Rule{{Method: "GET", Path: "/" + name}},
		})
		if err != nil {
			return
		}
		d := Decide(spec, PacketMeta{DstIP: dstIP, DNSQName: qname, SNI: qname, HTTPMethod: "GET", HTTPPath: "/" + qname, Bytes: 1500})
		if d.Action != "allow" && d.Action != "drop" {
			t.Fatalf("unexpected action %q", d.Action)
		}
	})
}
