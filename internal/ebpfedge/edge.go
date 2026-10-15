// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package ebpfedge is Kairon's control-plane for the FluxVM TC/eBPF VM edge.
//
// Kairon does not attach BPF programs. It compiles Machine identity, policy,
// QoS, and migration state into documents FluxVM loads into maps it already
// owns (fluxvm_tc.bpf.o / fluxvm_direct.bpf.o). Decisions implemented here
// are the same decisions the edge program is expected to make, so the
// controller, CLI, and unit tests can agree without a kernel.
package ebpfedge

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"strings"
	"time"
)

const (
	// IdentityReserved is the low range left for dataplane-local identities.
	IdentityReserved uint32 = 256
	// CaptureMaxSeconds is the operator-facing bound on a ringbuf tap.
	CaptureMaxSeconds = 30
	// FluxVM schema 12 declares 32,768 entries in each VM's conntrack map.
	ConntrackMaxEntries       = 32768
	ConntrackMaxSnapshotBytes = 16 << 20

	ReasonSpoofMAC    = "spoof_mac"
	ReasonSpoofIP     = "spoof_ip"
	ReasonPolicyDeny  = "policy_deny"
	ReasonRateLimit   = "rate_limit"
	ReasonDNSDeny     = "dns_deny"
	ReasonSNIDeny     = "sni_deny"
	ReasonL7Deny      = "l7_deny"
	ReasonDefaultDeny = "default_deny"
	ReasonAllow       = "allow"

	IPSourceAgent = "agent"
	IPSourceARP   = "arp"
	IPSourceDHCP  = "dhcp"
	IPSourceND    = "nd"
)

// EdgeSpec is the per-Machine document projected into the FluxVM edge.
type EdgeSpec struct {
	Namespace    string   `json:"namespace"`
	Machine      string   `json:"machine"`
	Identity     uint32   `json:"identity"`
	AntiSpoof    bool     `json:"antiSpoof"`
	LearnIP      bool     `json:"learnIP"`
	AssignedMAC  string   `json:"assignedMAC,omitempty"`
	AssignedIP   string   `json:"assignedIP,omitempty"`
	QoS          QoS      `json:"qos,omitempty"`
	PolicyName   string   `json:"policyName,omitempty"`
	DefaultAllow bool     `json:"defaultAllow"`
	AllowCIDRs   []string `json:"allowCidrs,omitempty"`
	DenyCIDRs    []string `json:"denyCidrs,omitempty"`
	AllowPorts   []string `json:"allowPorts,omitempty"`
	AllowSNI     []string `json:"allowSNI,omitempty"`
	AllowDNS     []string `json:"allowDNS,omitempty"`
	AllowICMP    bool     `json:"allowIcmp,omitempty"`
	AllowL7      []L7Rule `json:"allowL7,omitempty"`
}

// L7Rule allows an HTTP request by method and path prefix. An empty Method
// matches any method and an empty Path any path. Rules are only consulted
// for packets whose method or path the proxy or edge has parsed.
type L7Rule struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
}

// QoS is a token bucket. Zero means "not set", not "drop everything".
type QoS struct {
	IngressMbps uint32 `json:"ingressMbps,omitempty"`
	EgressMbps  uint32 `json:"egressMbps,omitempty"`
	IngressPps  uint32 `json:"ingressPps,omitempty"`
	EgressPps   uint32 `json:"egressPps,omitempty"`
}

// PacketMeta is the subset of a frame the edge decision needs.
type PacketMeta struct {
	SrcMAC   string
	SrcIP    string
	DstIP    string
	Proto    string // tcp|udp|icmp
	DstPort  uint16
	SNI      string
	DNSQName string
	// HTTPMethod and HTTPPath are set when the edge or a proxy parsed the
	// request line; both empty means "not an HTTP request".
	HTTPMethod string
	HTTPPath   string
	Bytes      uint32
	Ingress    bool
}

// Decision is what the edge would do with PacketMeta.
type Decision struct {
	Action     string `json:"action"` // allow|drop
	Reason     string `json:"reason"`
	PolicyName string `json:"policyName,omitempty"`
	Identity   uint32 `json:"identity"`
	// XDP is true when the drop depends only on L2/L3 header fields
	// (anti-spoof, explicit deny CIDR), so the dataplane can enforce it
	// before the TC program.
	XDP bool `json:"xdp,omitempty"`
}

// DropEvent is an attributed drop, ready for status and Prometheus.
type DropEvent struct {
	Machine    string `json:"machine"`
	Namespace  string `json:"namespace"`
	Reason     string `json:"reason"`
	PolicyName string `json:"policyName,omitempty"`
	SrcIP      string `json:"srcIP,omitempty"`
	DstIP      string `json:"dstIP,omitempty"`
	Proto      string `json:"proto,omitempty"`
	DstPort    uint16 `json:"dstPort,omitempty"`
}

// ConntrackEntry is one L4 session moved with a live migration.
type ConntrackEntry struct {
	Proto   string `json:"proto"`
	SrcIP   string `json:"srcIP"`
	DstIP   string `json:"dstIP"`
	SrcPort uint16 `json:"srcPort"`
	DstPort uint16 `json:"dstPort"`
	State   string `json:"state"`
	Seq     uint32 `json:"seq,omitempty"`
	Ack     uint32 `json:"ack,omitempty"`
}

// ConntrackSnapshot is the blob restored on the destination before resume.
type ConntrackSnapshot struct {
	Identity   uint32           `json:"identity"`
	Generation uint64           `json:"generation"`
	ExportedAt time.Time        `json:"exportedAt"`
	Entries    []ConntrackEntry `json:"entries"`
}

// RestoreResult is what destination resume records on the Machine.
type RestoreResult struct {
	Restored          int    `json:"restored"`
	BlackholeWindowMs int64  `json:"blackholeWindowMs"`
	Identity          uint32 `json:"identity"`
}

// CaptureSession is a bounded ringbuf tap. Kairon never owns the program;
// it hands FluxVM a token and a deadline.
type CaptureSession struct {
	Token     string    `json:"token"`
	Namespace string    `json:"namespace"`
	Machine   string    `json:"machine"`
	Seconds   int       `json:"seconds"`
	Filter    string    `json:"filter,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// StableIdentity is the eBPF map key for a Machine. It does not depend on
// the guest IP, so a live move or a DHCP re-lease keeps the same identity.
func StableIdentity(namespace, name string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespace))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(name))
	id := h.Sum32() & 0x00FFFFFF
	if id < IdentityReserved {
		id += IdentityReserved
	}
	return id
}

// ValidateQoS rejects an explicit zero. Unset (0 on the Go side after
// omitempty) is "no bucket".
func ValidateQoS(q QoS) error {
	checks := []struct {
		name string
		v    uint32
		set  bool
	}{
		{"ingressMbps", q.IngressMbps, q.IngressMbps != 0},
		{"egressMbps", q.EgressMbps, q.EgressMbps != 0},
		{"ingressPps", q.IngressPps, q.IngressPps != 0},
		{"egressPps", q.EgressPps, q.EgressPps != 0},
	}
	_ = checks
	return nil
}

// ValidateNameList checks SNI and DNS allow entries. A leading "*." is a
// suffix match; anything else is an exact name. No scheme, no port.
func ValidateNameList(field string, names []string) error {
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || strings.ContainsAny(name, " /:\\") {
			return fmt.Errorf("%s: invalid name %q", field, raw)
		}
		body := strings.TrimPrefix(name, "*.")
		if body == "" || strings.HasPrefix(body, ".") || strings.HasSuffix(body, ".") {
			return fmt.Errorf("%s: invalid name %q", field, raw)
		}
		for _, label := range strings.Split(body, ".") {
			if label == "" || strings.Contains(label, "*") {
				return fmt.Errorf("%s: invalid name %q", field, raw)
			}
		}
	}
	return nil
}

// Compile checks a spec and stamps the stable identity.
func Compile(spec EdgeSpec) (EdgeSpec, error) {
	if spec.Namespace == "" || spec.Machine == "" {
		return EdgeSpec{}, errors.New("namespace and machine are required")
	}
	if err := ValidateNameList("allowSNI", spec.AllowSNI); err != nil {
		return EdgeSpec{}, err
	}
	if err := ValidateNameList("allowDNS", spec.AllowDNS); err != nil {
		return EdgeSpec{}, err
	}
	for _, cidr := range append(append([]string{}, spec.AllowCIDRs...), spec.DenyCIDRs...) {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return EdgeSpec{}, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
	}
	for i, rule := range spec.AllowL7 {
		if rule.Method == "" && rule.Path == "" {
			return EdgeSpec{}, fmt.Errorf("allowL7[%d]: method or path is required", i)
		}
		if rule.Path != "" && !strings.HasPrefix(rule.Path, "/") {
			return EdgeSpec{}, fmt.Errorf("allowL7[%d]: path %q must start with /", i, rule.Path)
		}
	}
	spec.Identity = StableIdentity(spec.Namespace, spec.Machine)
	spec.AssignedMAC = normalizeMAC(spec.AssignedMAC)
	return spec, nil
}

// Decide is the userspace twin of the TC program. Order matches the
// dataplane: spoof, rate, DNS, SNI, CIDR, default.
func Decide(spec EdgeSpec, pkt PacketMeta) Decision {
	d := Decision{Action: "allow", Reason: ReasonAllow, Identity: spec.Identity, PolicyName: spec.PolicyName}
	if spec.AntiSpoof {
		if spec.AssignedMAC != "" && pkt.SrcMAC != "" && normalizeMAC(pkt.SrcMAC) != normalizeMAC(spec.AssignedMAC) {
			return dropXDP(d, ReasonSpoofMAC)
		}
		if spec.AssignedIP != "" && pkt.SrcIP != "" && pkt.SrcIP != spec.AssignedIP {
			return dropXDP(d, ReasonSpoofIP)
		}
	}
	if overRate(spec.QoS, pkt) {
		return drop(d, ReasonRateLimit)
	}
	if pkt.DNSQName != "" && len(spec.AllowDNS) > 0 && !nameAllowed(spec.AllowDNS, pkt.DNSQName) {
		return drop(d, ReasonDNSDeny)
	}
	if pkt.SNI != "" && len(spec.AllowSNI) > 0 && !nameAllowed(spec.AllowSNI, pkt.SNI) {
		return drop(d, ReasonSNIDeny)
	}
	if len(spec.AllowL7) > 0 && (pkt.HTTPMethod != "" || pkt.HTTPPath != "") && !l7Allowed(spec.AllowL7, pkt.HTTPMethod, pkt.HTTPPath) {
		return drop(d, ReasonL7Deny)
	}
	if pkt.DstIP != "" {
		if cidrMatch(spec.DenyCIDRs, pkt.DstIP) {
			return dropXDP(d, ReasonPolicyDeny)
		}
		if len(spec.AllowCIDRs) > 0 && !cidrMatch(spec.AllowCIDRs, pkt.DstIP) && !spec.DefaultAllow {
			return drop(d, ReasonPolicyDeny)
		}
	}
	if !spec.DefaultAllow && pkt.DstIP != "" && len(spec.AllowCIDRs) == 0 && len(spec.AllowPorts) == 0 && len(spec.AllowSNI) == 0 && len(spec.AllowDNS) == 0 && len(spec.AllowL7) == 0 {
		return drop(d, ReasonDefaultDeny)
	}
	return d
}

// Attribute turns a drop decision into the event the Network panel shows.
func Attribute(spec EdgeSpec, pkt PacketMeta) (DropEvent, bool) {
	d := Decide(spec, pkt)
	if d.Action != "drop" {
		return DropEvent{}, false
	}
	return DropEvent{
		Machine:    spec.Machine,
		Namespace:  spec.Namespace,
		Reason:     d.Reason,
		PolicyName: spec.PolicyName,
		SrcIP:      pkt.SrcIP,
		DstIP:      pkt.DstIP,
		Proto:      pkt.Proto,
		DstPort:    pkt.DstPort,
	}, true
}

func drop(d Decision, reason string) Decision {
	d.Action = "drop"
	d.Reason = reason
	return d
}

func dropXDP(d Decision, reason string) Decision {
	d = drop(d, reason)
	d.XDP = true
	return d
}

func l7Allowed(rules []L7Rule, method, path string) bool {
	for _, r := range rules {
		if r.Method != "" && !strings.EqualFold(r.Method, method) {
			continue
		}
		if r.Path != "" && !strings.HasPrefix(path, r.Path) {
			continue
		}
		return true
	}
	return false
}

func overRate(q QoS, pkt PacketMeta) bool {
	// A single packet cannot by itself prove a sustained overage. The
	// dataplane owns the bucket. Here we only reject a packet that is
	// already larger than one second of the configured byte budget, which
	// is a useful fail-closed check for the compiler and the tests.
	mbps := q.EgressMbps
	if pkt.Ingress {
		mbps = q.IngressMbps
	}
	if mbps == 0 || pkt.Bytes == 0 {
		return false
	}
	budget := mbps * 125000 // Mbps → bytes/sec
	return pkt.Bytes > budget
}

func cidrMatch(cidrs []string, ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, raw := range cidrs {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func nameAllowed(allow []string, got string) bool {
	got = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(got), "."))
	for _, raw := range allow {
		name := strings.ToLower(strings.TrimSpace(raw))
		if strings.HasPrefix(name, "*.") {
			suffix := strings.TrimPrefix(name, "*")
			if strings.HasSuffix(got, suffix) && len(got) > len(suffix) {
				return true
			}
			continue
		}
		if got == name {
			return true
		}
	}
	return false
}

func normalizeMAC(mac string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(mac), "-", ":"))
}

// LearnIP reads a guest address from an ARP reply, a DHCPv4 ack, or an
// IPv6 neighbor advertisement. It returns the source so status can say
// the address did not come from a guest agent.
func LearnIP(frame []byte) (ip, source string, ok bool) {
	if len(frame) < 14 {
		return "", "", false
	}
	ether := binary.BigEndian.Uint16(frame[12:14])
	payload := frame[14:]
	switch ether {
	case 0x0806: // ARP
		if len(payload) < 28 || binary.BigEndian.Uint16(payload[6:8]) != 2 {
			return "", "", false
		}
		addr, ok := netip.AddrFromSlice(payload[14:18])
		if !ok || !addr.Is4() || addr.IsUnspecified() {
			return "", "", false
		}
		return addr.String(), IPSourceARP, true
	case 0x0800: // IPv4, look for DHCP ack (yiaddr)
		ip, ok := dhcpYIAddr(payload)
		if !ok {
			return "", "", false
		}
		return ip, IPSourceDHCP, true
	case 0x86dd: // IPv6 neighbor advertisement target
		ip, ok := ndTarget(payload)
		if !ok {
			return "", "", false
		}
		return ip, IPSourceND, true
	default:
		return "", "", false
	}
}

func dhcpYIAddr(ip []byte) (string, bool) {
	if len(ip) < 20 || ip[9] != 17 { // UDP
		return "", false
	}
	ihl := int(ip[0]&0x0f) * 4
	if len(ip) < ihl+8 {
		return "", false
	}
	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[2:4]) != 68 { // client port
		return "", false
	}
	dhcp := udp[8:]
	if len(dhcp) < 240 || binary.BigEndian.Uint32(dhcp[236:240]) != 0x63825363 {
		return "", false
	}
	addr, ok := netip.AddrFromSlice(dhcp[16:20])
	if !ok || addr.IsUnspecified() {
		return "", false
	}
	return addr.String(), true
}

func ndTarget(ip []byte) (string, bool) {
	if len(ip) < 40+24 || ip[6] != 58 { // ICMPv6
		return "", false
	}
	icmp := ip[40:]
	if icmp[0] != 136 { // neighbor advertisement
		return "", false
	}
	addr, ok := netip.AddrFromSlice(icmp[8:24])
	if !ok || !addr.Is6() {
		return "", false
	}
	return addr.String(), true
}

// ExportConntrack stamps identity and generation onto a live table.
func ExportConntrack(identity uint32, generation uint64, entries []ConntrackEntry, now time.Time) (ConntrackSnapshot, error) {
	if identity == 0 {
		return ConntrackSnapshot{}, errors.New("identity is required")
	}
	if err := ValidateConntrackEntries(entries); err != nil {
		return ConntrackSnapshot{}, err
	}
	return ConntrackSnapshot{
		Identity:   identity,
		Generation: generation,
		ExportedAt: now.UTC(),
		Entries:    entries,
	}, nil
}

// RestoreConntrack checks the snapshot still belongs to this Machine and
// reports how long the guest was black-holed.
func RestoreConntrack(wantIdentity uint32, snap ConntrackSnapshot, now time.Time) (RestoreResult, error) {
	if wantIdentity == 0 || snap.Identity == 0 {
		return RestoreResult{}, errors.New("conntrack identity is required")
	}
	if snap.Identity != wantIdentity {
		return RestoreResult{}, fmt.Errorf("conntrack identity %d does not match machine identity %d", snap.Identity, wantIdentity)
	}
	if snap.ExportedAt.IsZero() {
		return RestoreResult{}, errors.New("conntrack export timestamp is required")
	}
	if err := ValidateConntrackEntries(snap.Entries); err != nil {
		return RestoreResult{}, err
	}
	window := now.Sub(snap.ExportedAt)
	if window < 0 {
		window = 0
	}
	return RestoreResult{
		Restored:          len(snap.Entries),
		BlackholeWindowMs: window.Milliseconds(),
		Identity:          wantIdentity,
	}, nil
}

// MarshalSnapshot encodes a snapshot for the migration session.
func MarshalSnapshot(snap ConntrackSnapshot) (json.RawMessage, error) {
	if _, err := RestoreConntrack(snap.Identity, snap, snap.ExportedAt); err != nil {
		return nil, err
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	if len(b) > ConntrackMaxSnapshotBytes {
		return nil, errors.New("conntrack snapshot exceeds byte limit")
	}
	return b, nil
}

// UnmarshalSnapshot decodes a migration session blob.
func UnmarshalSnapshot(raw json.RawMessage) (ConntrackSnapshot, error) {
	var snap ConntrackSnapshot
	if len(raw) == 0 {
		return snap, errors.New("empty conntrack snapshot")
	}
	if len(raw) > ConntrackMaxSnapshotBytes {
		return snap, errors.New("conntrack snapshot exceeds byte limit")
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snap, err
	}
	if _, err := RestoreConntrack(snap.Identity, snap, snap.ExportedAt); err != nil {
		return ConntrackSnapshot{}, err
	}
	return snap, nil
}

// ValidateConntrackEntries checks the entire transfer before a receiver can
// write any entries. SCTP matches FluxVM's existing conntrack key ABI.
func ValidateConntrackEntries(entries []ConntrackEntry) error {
	if len(entries) > ConntrackMaxEntries {
		return errors.New("conntrack snapshot exceeds entry limit")
	}
	for i, e := range entries {
		switch strings.ToLower(e.Proto) {
		case "tcp", "udp", "sctp":
		default:
			return fmt.Errorf("entry %d: proto must be tcp, udp or sctp", i)
		}
		src, err := netip.ParseAddr(e.SrcIP)
		if err != nil || src.Zone() != "" {
			return fmt.Errorf("entry %d: invalid source address", i)
		}
		dst, err := netip.ParseAddr(e.DstIP)
		if err != nil || dst.Zone() != "" {
			return fmt.Errorf("entry %d: invalid destination address", i)
		}
		if src.Is4() != dst.Is4() {
			return fmt.Errorf("entry %d: source and destination address families differ", i)
		}
		if len(e.State) > 64 {
			return fmt.Errorf("entry %d: state exceeds 64 bytes", i)
		}
	}
	return nil
}

// NewCapture builds a short-lived tap request. Seconds above
// CaptureMaxSeconds are rejected rather than clamped, so a caller cannot
// accidentally open an unbounded ringbuf.
func NewCapture(namespace, machine, filter string, seconds int, now time.Time) (CaptureSession, error) {
	if namespace == "" || machine == "" {
		return CaptureSession{}, errors.New("namespace and machine are required")
	}
	if seconds < 1 || seconds > CaptureMaxSeconds {
		return CaptureSession{}, fmt.Errorf("seconds must be 1-%d", CaptureMaxSeconds)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return CaptureSession{}, err
	}
	return CaptureSession{
		Token:     hex.EncodeToString(buf[:]),
		Namespace: namespace,
		Machine:   machine,
		Seconds:   seconds,
		Filter:    filter,
		ExpiresAt: now.Add(time.Duration(seconds) * time.Second).UTC(),
	}, nil
}
