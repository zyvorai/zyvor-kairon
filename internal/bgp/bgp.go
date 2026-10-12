// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package bgp is a minimal, announce-only BGP-4 speaker (RFC 4271): it opens
// a session to each configured peer, advertises a set of IPv4 prefixes with
// this node as next hop and withdraws them when they go away. It never
// accepts routes: UPDATEs from peers are read and discarded. kairon-node uses
// it to announce floating IPs and Service Fabric VIPs to top-of-rack routers.
//
// Standard library only (docs/DEPENDENCIES.md), so it deliberately supports
// just what announcing needs: IPv4 unicast, 4-octet AS numbers (RFC 6793),
// eBGP and iBGP, no TCP-MD5, no graceful restart, no route refresh.
package bgp

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	msgOpen         = 1
	msgUpdate       = 2
	msgNotification = 3
	msgKeepalive    = 4

	headerLen = 19
	maxMsgLen = 4096

	asTrans = 23456

	attrOrigin    = 1
	attrASPath    = 2
	attrNextHop   = 3
	attrLocalPref = 5

	flagTransitive = 0x40

	defaultHold = 90 * time.Second
	defaultPort = 179
)

// Peer is one BGP neighbor.
type Peer struct {
	Addr netip.Addr
	Port int
	ASN  uint32
}

func (p Peer) String() string {
	return net.JoinHostPort(p.Addr.String(), strconv.Itoa(p.port()))
}

func (p Peer) port() int {
	if p.Port == 0 {
		return defaultPort
	}
	return p.Port
}

// ParsePeers parses "addr=asn[,addr:port=asn...]".
func ParsePeers(s string) ([]Peer, error) {
	var out []Peer
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		host, asnStr, ok := strings.Cut(item, "=")
		if !ok {
			return nil, fmt.Errorf("BGP peer %q: want addr=asn", item)
		}
		asn, err := strconv.ParseUint(asnStr, 10, 32)
		if err != nil || asn == 0 {
			return nil, fmt.Errorf("BGP peer %q: invalid ASN", item)
		}
		p := Peer{ASN: uint32(asn)}
		if ap, err := netip.ParseAddrPort(host); err == nil {
			p.Addr, p.Port = ap.Addr(), int(ap.Port())
		} else if a, err := netip.ParseAddr(host); err == nil {
			p.Addr = a
		} else {
			return nil, fmt.Errorf("BGP peer %q: invalid address", item)
		}
		if !p.Addr.Is4() {
			return nil, fmt.Errorf("BGP peer %q: only IPv4 peers are supported", item)
		}
		out = append(out, p)
	}
	return out, nil
}

// Config is a speaker's identity.
type Config struct {
	LocalASN uint32
	RouterID netip.Addr
	// NextHop is the address peers forward announced prefixes to.
	NextHop netip.Addr
	Peers   []Peer
	Hold    time.Duration
	Log     *slog.Logger
}

// Speaker announces one set of prefixes to every peer.
type Speaker struct {
	cfg Config

	mu       sync.Mutex
	routes   map[netip.Prefix]bool
	changed  chan struct{}
	sessions map[string]*SessionState
}

// SessionState is what Status reports about one peer.
type SessionState struct {
	Peer        string    `json:"peer"`
	State       string    `json:"state"` // Idle|Connect|OpenSent|Established
	Since       time.Time `json:"since"`
	LastError   string    `json:"lastError,omitempty"`
	Advertised  int       `json:"advertised"`
	FourOctetAS bool      `json:"fourOctetAS"`
}

// New validates cfg and returns an idle speaker; Run starts it.
func New(cfg Config) (*Speaker, error) {
	if cfg.LocalASN == 0 {
		return nil, errors.New("BGP: local ASN is required")
	}
	if !cfg.RouterID.Is4() {
		return nil, errors.New("BGP: an IPv4 router ID is required")
	}
	if !cfg.NextHop.IsValid() {
		cfg.NextHop = cfg.RouterID
	}
	if !cfg.NextHop.Is4() {
		return nil, errors.New("BGP: the next hop must be IPv4")
	}
	if len(cfg.Peers) == 0 {
		return nil, errors.New("BGP: no peers")
	}
	if cfg.Hold == 0 {
		cfg.Hold = defaultHold
	}
	if cfg.Hold < 3*time.Second {
		return nil, errors.New("BGP: hold time must be at least 3s")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Speaker{cfg: cfg, routes: map[netip.Prefix]bool{}, changed: make(chan struct{}), sessions: map[string]*SessionState{}}
	for _, p := range cfg.Peers {
		s.sessions[p.String()] = &SessionState{Peer: p.String(), State: "Idle", Since: time.Now()}
	}
	return s, nil
}

// SetRoutes replaces the announced prefixes; IPv6 prefixes are ignored.
func (s *Speaker) SetRoutes(prefixes []netip.Prefix) {
	next := map[netip.Prefix]bool{}
	for _, p := range prefixes {
		if p.Addr().Is4() {
			next[p.Masked()] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mapsEqual(next, s.routes) {
		return
	}
	s.routes = next
	close(s.changed)
	s.changed = make(chan struct{})
}

func mapsEqual(a, b map[netip.Prefix]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (s *Speaker) snapshot() (map[netip.Prefix]bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[netip.Prefix]bool, len(s.routes))
	for k := range s.routes {
		out[k] = true
	}
	return out, s.changed
}

// Status reports every peer session, sorted by peer.
func (s *Speaker) Status() []SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionState, 0, len(s.sessions))
	for _, st := range s.sessions {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

func (s *Speaker) setState(peer, state string, err error, advertised int, fourOctet bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sessions[peer]
	if st.State != state {
		st.Since = time.Now()
	}
	st.State = state
	if err != nil {
		st.LastError = err.Error()
	}
	if advertised >= 0 {
		st.Advertised = advertised
	}
	st.FourOctetAS = fourOctet
}

// Run keeps a session to every peer until ctx ends, reconnecting with backoff.
func (s *Speaker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range s.cfg.Peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			backoff := time.Second
			for ctx.Err() == nil {
				start := time.Now()
				err := s.session(ctx, p)
				if ctx.Err() != nil {
					break
				}
				s.setState(p.String(), "Idle", err, 0, false)
				s.cfg.Log.Warn("BGP session ended", "peer", p.String(), "error", err)
				if time.Since(start) > time.Minute {
					backoff = time.Second
				}
				select {
				case <-ctx.Done():
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
			s.setState(p.String(), "Idle", nil, 0, false)
		}(p)
	}
	wg.Wait()
}

func (s *Speaker) session(ctx context.Context, p Peer) error {
	s.setState(p.String(), "Connect", nil, -1, false)
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp4", p.String())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if _, err := conn.Write(encodeOpen(s.cfg.LocalASN, s.cfg.RouterID, s.cfg.Hold)); err != nil {
		return err
	}
	s.setState(p.String(), "OpenSent", nil, -1, false)
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Hold))
	typ, body, err := readMessage(r)
	if err != nil {
		return err
	}
	if typ == msgNotification {
		return notificationError(body)
	}
	if typ != msgOpen {
		return fmt.Errorf("expected OPEN, got message type %d", typ)
	}
	open, err := decodeOpen(body)
	if err != nil {
		_, _ = conn.Write(encodeNotification(2, 0))
		return err
	}
	if open.ASN != p.ASN {
		_, _ = conn.Write(encodeNotification(2, 2)) // Bad Peer AS
		return fmt.Errorf("peer AS %d, configured %d", open.ASN, p.ASN)
	}
	hold := s.cfg.Hold
	if open.Hold != 0 && open.Hold < hold {
		hold = open.Hold
	}
	keepalive := hold / 3
	if open.Hold == 0 {
		keepalive = 0
	}
	if _, err := conn.Write(encodeKeepalive()); err != nil {
		return err
	}
	fourOctet := open.FourOctetAS
	ibgp := p.ASN == s.cfg.LocalASN

	errCh := make(chan error, 1)
	// The peer's first KEEPALIVE (or UPDATE) confirms our OPEN.
	confirmed := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			if open.Hold != 0 {
				_ = conn.SetReadDeadline(time.Now().Add(hold))
			} else {
				_ = conn.SetReadDeadline(time.Time{})
			}
			typ, body, err := readMessage(r)
			if err != nil {
				errCh <- err
				return
			}
			switch typ {
			case msgNotification:
				errCh <- notificationError(body)
				return
			case msgKeepalive, msgUpdate:
				// Routes from peers are not accepted.
				once.Do(func() { close(confirmed) })
			default:
				errCh <- fmt.Errorf("unexpected message type %d", typ)
				return
			}
		}
	}()

	var tick <-chan time.Time
	if keepalive > 0 {
		t := time.NewTicker(keepalive)
		defer t.Stop()
		tick = t.C
	}
	select {
	case <-confirmed:
	case err := <-errCh:
		return err
	case <-time.After(hold):
		return errors.New("no KEEPALIVE after OPEN")
	case <-ctx.Done():
		return ctx.Err()
	}
	s.cfg.Log.Info("BGP session established", "peer", p.String(), "asn", p.ASN, "fourOctetAS", fourOctet)
	s.setState(p.String(), "Established", nil, 0, fourOctet)

	announced := map[netip.Prefix]bool{}
	for {
		routes, changed := s.snapshot()
		var add, del []netip.Prefix
		for p := range routes {
			if !announced[p] {
				add = append(add, p)
			}
		}
		for p := range announced {
			if !routes[p] {
				del = append(del, p)
			}
		}
		for _, msg := range encodeUpdates(add, del, s.cfg.LocalASN, s.cfg.NextHop, fourOctet, ibgp) {
			if _, err := conn.Write(msg); err != nil {
				return err
			}
		}
		announced = routes
		s.setState(p.String(), "Established", nil, len(announced), fourOctet)
		select {
		case err := <-errCh:
			return err
		case <-changed:
		case <-tick:
			if _, err := conn.Write(encodeKeepalive()); err != nil {
				return err
			}
		case <-ctx.Done():
			_, _ = conn.Write(encodeNotification(6, 2)) // Cease, Administrative Shutdown
			return ctx.Err()
		}
	}
}

func notificationError(body []byte) error {
	if len(body) < 2 {
		return errors.New("NOTIFICATION from peer")
	}
	return fmt.Errorf("NOTIFICATION from peer: code %d subcode %d", body[0], body[1])
}

func header(typ byte, bodyLen int) []byte {
	b := make([]byte, headerLen, headerLen+bodyLen)
	for i := 0; i < 16; i++ {
		b[i] = 0xff
	}
	binary.BigEndian.PutUint16(b[16:], uint16(headerLen+bodyLen))
	b[18] = typ
	return b
}

func readMessage(r io.Reader) (byte, []byte, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	for i := 0; i < 16; i++ {
		if h[i] != 0xff {
			return 0, nil, errors.New("BGP header marker is not all ones")
		}
	}
	n := int(binary.BigEndian.Uint16(h[16:]))
	if n < headerLen || n > maxMsgLen {
		return 0, nil, fmt.Errorf("BGP message length %d", n)
	}
	body := make([]byte, n-headerLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return h[18], body, nil
}

func encodeKeepalive() []byte { return header(msgKeepalive, 0) }

func encodeNotification(code, sub byte) []byte {
	return append(header(msgNotification, 2), code, sub)
}

// encodeOpen advertises IPv4 unicast (RFC 4760) and 4-octet AS (RFC 6793).
func encodeOpen(asn uint32, id netip.Addr, hold time.Duration) []byte {
	caps := []byte{
		1, 4, 0, 1, 0, 1, // multiprotocol: AFI 1 (IPv4), SAFI 1 (unicast)
		65, 4, byte(asn >> 24), byte(asn >> 16), byte(asn >> 8), byte(asn),
	}
	params := append([]byte{2, byte(len(caps))}, caps...)
	my := uint16(asTrans)
	if asn <= 0xffff {
		my = uint16(asn)
	}
	body := make([]byte, 0, 10+len(params))
	body = append(body, 4)
	body = binary.BigEndian.AppendUint16(body, my)
	body = binary.BigEndian.AppendUint16(body, uint16(hold/time.Second))
	a := id.As4()
	body = append(body, a[:]...)
	body = append(body, byte(len(params)))
	body = append(body, params...)
	return append(header(msgOpen, len(body)), body...)
}

type openMsg struct {
	ASN         uint32
	Hold        time.Duration
	RouterID    netip.Addr
	FourOctetAS bool
}

func decodeOpen(b []byte) (openMsg, error) {
	if len(b) < 10 {
		return openMsg{}, errors.New("short OPEN")
	}
	if b[0] != 4 {
		return openMsg{}, fmt.Errorf("BGP version %d", b[0])
	}
	o := openMsg{
		ASN:      uint32(binary.BigEndian.Uint16(b[1:])),
		Hold:     time.Duration(binary.BigEndian.Uint16(b[3:])) * time.Second,
		RouterID: netip.AddrFrom4([4]byte{b[5], b[6], b[7], b[8]}),
	}
	if o.Hold != 0 && o.Hold < 3*time.Second {
		return openMsg{}, fmt.Errorf("unacceptable hold time %s", o.Hold)
	}
	params := b[10:]
	if int(b[9]) != len(params) {
		return openMsg{}, errors.New("OPEN parameter length mismatch")
	}
	for len(params) >= 2 {
		typ, n := params[0], int(params[1])
		if len(params) < 2+n {
			return openMsg{}, errors.New("truncated OPEN parameter")
		}
		val := params[2 : 2+n]
		params = params[2+n:]
		if typ != 2 {
			continue
		}
		for len(val) >= 2 {
			code, cn := val[0], int(val[1])
			if len(val) < 2+cn {
				return openMsg{}, errors.New("truncated capability")
			}
			if code == 65 && cn == 4 {
				o.FourOctetAS = true
				o.ASN = binary.BigEndian.Uint32(val[2:6])
			}
			val = val[2+cn:]
		}
	}
	return o, nil
}

func appendPrefix(b []byte, p netip.Prefix) []byte {
	a := p.Addr().As4()
	n := (p.Bits() + 7) / 8
	b = append(b, byte(p.Bits()))
	return append(b, a[:n]...)
}

// encodeUpdates builds UPDATE messages withdrawing del and announcing add,
// split so none exceeds the 4096-byte message limit.
func encodeUpdates(add, del []netip.Prefix, asn uint32, nextHop netip.Addr, fourOctet, ibgp bool) [][]byte {
	sortPrefixes(add)
	sortPrefixes(del)
	var out [][]byte
	const room = maxMsgLen - headerLen - 64
	for len(del) > 0 {
		var w []byte
		for len(del) > 0 && len(w)+5 < room {
			w = appendPrefix(w, del[0])
			del = del[1:]
		}
		body := binary.BigEndian.AppendUint16(nil, uint16(len(w)))
		body = append(body, w...)
		body = binary.BigEndian.AppendUint16(body, 0)
		out = append(out, append(header(msgUpdate, len(body)), body...))
	}
	if len(add) == 0 {
		return out
	}
	attrs := []byte{flagTransitive, attrOrigin, 1, 0} // IGP
	switch {
	case ibgp:
		attrs = append(attrs, flagTransitive, attrASPath, 0)
		attrs = append(attrs, flagTransitive, attrLocalPref, 4, 0, 0, 0, 100)
	case fourOctet:
		attrs = append(attrs, flagTransitive, attrASPath, 6, 2, 1, byte(asn>>24), byte(asn>>16), byte(asn>>8), byte(asn))
	default:
		my := uint16(asTrans)
		if asn <= 0xffff {
			my = uint16(asn)
		}
		attrs = append(attrs, flagTransitive, attrASPath, 4, 2, 1, byte(my>>8), byte(my))
	}
	nh := nextHop.As4()
	attrs = append(attrs, flagTransitive, attrNextHop, 4, nh[0], nh[1], nh[2], nh[3])
	for len(add) > 0 {
		var nlri []byte
		for len(add) > 0 && len(nlri)+len(attrs)+5 < room {
			nlri = appendPrefix(nlri, add[0])
			add = add[1:]
		}
		body := binary.BigEndian.AppendUint16(nil, 0)
		body = binary.BigEndian.AppendUint16(body, uint16(len(attrs)))
		body = append(body, attrs...)
		body = append(body, nlri...)
		out = append(out, append(header(msgUpdate, len(body)), body...))
	}
	return out
}

func sortPrefixes(ps []netip.Prefix) {
	sort.Slice(ps, func(i, j int) bool {
		if c := ps[i].Addr().Compare(ps[j].Addr()); c != 0 {
			return c < 0
		}
		return ps[i].Bits() < ps[j].Bits()
	})
}

// update is a decoded UPDATE, for tests and diagnostics.
type update struct {
	Withdrawn []netip.Prefix
	NLRI      []netip.Prefix
	ASPath    []uint32
	NextHop   netip.Addr
	LocalPref uint32
}

func decodePrefixes(b []byte) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for len(b) > 0 {
		bits := int(b[0])
		n := (bits + 7) / 8
		if bits > 32 || len(b) < 1+n {
			return nil, errors.New("bad prefix")
		}
		var a [4]byte
		copy(a[:], b[1:1+n])
		out = append(out, netip.PrefixFrom(netip.AddrFrom4(a), bits))
		b = b[1+n:]
	}
	return out, nil
}

func decodeUpdate(b []byte, fourOctet bool) (update, error) {
	var u update
	if len(b) < 4 {
		return u, errors.New("short UPDATE")
	}
	wl := int(binary.BigEndian.Uint16(b))
	if len(b) < 2+wl+2 {
		return u, errors.New("bad withdrawn length")
	}
	var err error
	if u.Withdrawn, err = decodePrefixes(b[2 : 2+wl]); err != nil {
		return u, err
	}
	rest := b[2+wl:]
	al := int(binary.BigEndian.Uint16(rest))
	if len(rest) < 2+al {
		return u, errors.New("bad attribute length")
	}
	attrs := rest[2 : 2+al]
	if u.NLRI, err = decodePrefixes(rest[2+al:]); err != nil {
		return u, err
	}
	for len(attrs) >= 3 {
		flags, typ := attrs[0], attrs[1]
		n, off := int(attrs[2]), 3
		if flags&0x10 != 0 {
			if len(attrs) < 4 {
				return u, errors.New("truncated attribute")
			}
			n, off = int(binary.BigEndian.Uint16(attrs[2:])), 4
		}
		if len(attrs) < off+n {
			return u, errors.New("truncated attribute")
		}
		v := attrs[off : off+n]
		attrs = attrs[off+n:]
		switch typ {
		case attrNextHop:
			if n == 4 {
				u.NextHop = netip.AddrFrom4([4]byte{v[0], v[1], v[2], v[3]})
			}
		case attrLocalPref:
			if n == 4 {
				u.LocalPref = binary.BigEndian.Uint32(v)
			}
		case attrASPath:
			size := 2
			if fourOctet {
				size = 4
			}
			for len(v) >= 2 {
				cnt := int(v[1])
				v = v[2:]
				for i := 0; i < cnt && len(v) >= size; i++ {
					if size == 4 {
						u.ASPath = append(u.ASPath, binary.BigEndian.Uint32(v))
					} else {
						u.ASPath = append(u.ASPath, uint32(binary.BigEndian.Uint16(v)))
					}
					v = v[size:]
				}
			}
		}
	}
	return u, nil
}
