// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package bgp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestParsePeers(t *testing.T) {
	got, err := ParsePeers("192.0.2.1=65001, 192.0.2.2:1790=4200000000")
	if err != nil {
		t.Fatal(err)
	}
	want := []Peer{
		{Addr: netip.MustParseAddr("192.0.2.1"), ASN: 65001},
		{Addr: netip.MustParseAddr("192.0.2.2"), Port: 1790, ASN: 4200000000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"192.0.2.1", "192.0.2.1=0", "x=65001", "2001:db8::1=65001", "192.0.2.1=abc"} {
		if _, err := ParsePeers(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if p, _ := ParsePeers(""); len(p) != 0 {
		t.Fatal("empty list must parse to no peers")
	}
}

func TestOpenRoundTrip(t *testing.T) {
	for _, asn := range []uint32{65000, 4200000000} {
		msg := encodeOpen(asn, netip.MustParseAddr("192.0.2.9"), 90*time.Second)
		typ, body, err := readMessage(bufio.NewReader(&byteReader{b: msg}))
		if err != nil || typ != msgOpen {
			t.Fatalf("readMessage: %v %d", err, typ)
		}
		o, err := decodeOpen(body)
		if err != nil {
			t.Fatal(err)
		}
		if o.ASN != asn || !o.FourOctetAS || o.Hold != 90*time.Second || o.RouterID != netip.MustParseAddr("192.0.2.9") {
			t.Fatalf("asn %d: got %+v", asn, o)
		}
	}
}

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func TestUpdatesEncodeAttributesAndSplit(t *testing.T) {
	nh := netip.MustParseAddr("192.0.2.9")
	add := []netip.Prefix{netip.MustParsePrefix("203.0.113.10/32"), netip.MustParsePrefix("198.51.100.0/24")}
	msgs := encodeUpdates(add, nil, 65000, nh, true, false)
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	u, err := decodeUpdate(msgs[0][headerLen:], true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(u.NLRI, []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.10/32")}) {
		t.Fatalf("NLRI %v", u.NLRI)
	}
	if !reflect.DeepEqual(u.ASPath, []uint32{65000}) || u.NextHop != nh || u.LocalPref != 0 {
		t.Fatalf("eBGP attributes %+v", u)
	}

	ibgp, _ := decodeUpdate(encodeUpdates(add[:1], nil, 65000, nh, true, true)[0][headerLen:], true)
	if len(ibgp.ASPath) != 0 || ibgp.LocalPref != 100 {
		t.Fatalf("iBGP attributes %+v", ibgp)
	}
	two, _ := decodeUpdate(encodeUpdates(add[:1], nil, 4200000000, nh, false, false)[0][headerLen:], false)
	if !reflect.DeepEqual(two.ASPath, []uint32{asTrans}) {
		t.Fatalf("a 4-octet AS without the capability goes out as AS_TRANS: %v", two.ASPath)
	}

	var many []netip.Prefix
	for i := 0; i < 2000; i++ {
		many = append(many, netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1}), 32))
	}
	msgs = encodeUpdates(many, many[:1500], 65000, nh, true, false)
	var gotAdd, gotDel int
	for _, m := range msgs {
		if len(m) > maxMsgLen {
			t.Fatalf("message of %d bytes", len(m))
		}
		u, err := decodeUpdate(m[headerLen:], true)
		if err != nil {
			t.Fatal(err)
		}
		gotAdd += len(u.NLRI)
		gotDel += len(u.Withdrawn)
	}
	if gotAdd != 2000 || gotDel != 1500 {
		t.Fatalf("split lost prefixes: %d/%d", gotAdd, gotDel)
	}
}

// fakePeer accepts one session, answers OPEN, and reports UPDATEs.
func fakePeer(t *testing.T, ln net.Listener, asn uint32, updates chan<- update, opens chan<- openMsg) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	typ, body, err := readMessage(r)
	if err != nil || typ != msgOpen {
		t.Errorf("fake peer: first message %d %v", typ, err)
		return
	}
	o, err := decodeOpen(body)
	if err != nil {
		t.Errorf("fake peer: %v", err)
		return
	}
	opens <- o
	_, _ = conn.Write(encodeOpen(asn, netip.MustParseAddr("192.0.2.254"), 9*time.Second))
	_, _ = conn.Write(encodeKeepalive())
	for {
		typ, body, err := readMessage(r)
		if err != nil {
			return
		}
		switch typ {
		case msgUpdate:
			u, err := decodeUpdate(body, true)
			if err != nil {
				t.Errorf("fake peer: %v", err)
				return
			}
			updates <- u
		case msgKeepalive:
			_, _ = conn.Write(encodeKeepalive())
		case msgNotification:
			return
		}
	}
}

func TestSpeakerAnnouncesAndWithdraws(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	updates := make(chan update, 8)
	opens := make(chan openMsg, 1)
	go fakePeer(t, ln, 65001, updates, opens)

	s, err := New(Config{
		LocalASN: 65000,
		RouterID: netip.MustParseAddr("192.0.2.9"),
		NextHop:  netip.MustParseAddr("192.0.2.9"),
		Peers:    []Peer{{Addr: netip.MustParseAddr("127.0.0.1"), Port: port, ASN: 65001}},
		Hold:     9 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	fip := netip.MustParsePrefix("203.0.113.10/32")
	s.SetRoutes([]netip.Prefix{fip, netip.MustParsePrefix("2001:db8::/64")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	select {
	case o := <-opens:
		if o.ASN != 65000 || o.RouterID != netip.MustParseAddr("192.0.2.9") {
			t.Fatalf("OPEN %+v", o)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OPEN")
	}
	next := func() update {
		select {
		case u := <-updates:
			return u
		case <-time.After(5 * time.Second):
			t.Fatal("no UPDATE")
			return update{}
		}
	}
	u := next()
	if !reflect.DeepEqual(u.NLRI, []netip.Prefix{fip}) || u.NextHop != netip.MustParseAddr("192.0.2.9") {
		t.Fatalf("announce %+v (IPv6 must be dropped)", u)
	}
	// Status is recorded after the UPDATE is written; give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := s.Status()
		if len(st) == 1 && st[0].State == "Established" && st[0].Advertised == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.SetRoutes(nil)
	u = next()
	if !reflect.DeepEqual(u.Withdrawn, []netip.Prefix{fip}) || len(u.NLRI) != 0 {
		t.Fatalf("withdraw %+v", u)
	}
}

func TestSpeakerRefusesTheWrongPeerAS(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener:", err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	go fakePeer(t, ln, 65099, make(chan update, 1), make(chan openMsg, 1))
	s, _ := New(Config{
		LocalASN: 65000,
		RouterID: netip.MustParseAddr("192.0.2.9"),
		Peers:    []Peer{{Addr: netip.MustParseAddr("127.0.0.1"), Port: port, ASN: 65001}},
		Hold:     9 * time.Second,
	})
	err = s.session(context.Background(), s.cfg.Peers[0])
	if err == nil || s.Status()[0].State == "Established" {
		t.Fatalf("a peer with the wrong AS must not establish: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	id := netip.MustParseAddr("192.0.2.9")
	peers := []Peer{{Addr: id, ASN: 1}}
	for name, cfg := range map[string]Config{
		"no ASN":   {RouterID: id, Peers: peers},
		"no ID":    {LocalASN: 1, Peers: peers},
		"no peers": {LocalASN: 1, RouterID: id},
		"v6 hop":   {LocalASN: 1, RouterID: id, NextHop: netip.MustParseAddr("2001:db8::1"), Peers: peers},
		"hold":     {LocalASN: 1, RouterID: id, Peers: peers, Hold: time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
