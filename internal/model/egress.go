// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Egress modes of spec.network.egress, matching FluxVM's QemuEgressPolicy.
const (
	EgressLoopback  = "loopback"
	EgressAllowlist = "allowlist"
	EgressAudit     = "audit"
	EgressOpen      = "open"
)

// Limits shared with FluxVM (crates/fluxvm-network/src/qemu_egress.rs).
const (
	MaxEgressRules        = 256
	MaxEgressPortsPerCIDR = 4
)

// AnnotationEgressManaged marks a Machine whose FluxVM egress policy kairon-node has written from
// spec.network.egress. Dropping the field only clears the FluxVM policy when this is set, so a
// policy an operator set directly on FluxVM is never removed by a Machine that never asked for one.
const AnnotationEgressManaged = "kairon.zyvor.dev/egress-managed"

// EgressRule allows one destination. Port 0 means every port; a port needs proto tcp or udp.
type EgressRule struct {
	// CIDR is an IPv4 network or address ("10.0.0.0/8", "192.168.1.5").
	CIDR  string `json:"cidr"`
	Proto string `json:"proto,omitempty"` // any (default) | tcp | udp
	Port  uint16 `json:"port,omitempty"`
	Note  string `json:"note,omitempty"`
}

// EgressSpec is the outbound policy of the Machine's QEMU process. A guest on user-mode
// networking has no host-visible interface, so its traffic leaves through that process and this
// is the only policy that governs it. Empty mode means allowlist when rules are given, loopback
// (nothing leaves) otherwise.
type EgressSpec struct {
	Mode  string       `json:"mode,omitempty"` // loopback | allowlist | audit | open
	Rules []EgressRule `json:"rules,omitempty"`
}

// EffectiveMode resolves the default mode.
func (e EgressSpec) EffectiveMode() string {
	if e.Mode != "" {
		return e.Mode
	}
	if len(e.Rules) > 0 {
		return EgressAllowlist
	}
	return EgressLoopback
}

// ParseEgressCIDR parses an IPv4 network or bare address the way FluxVM does.
func ParseEgressCIDR(s string) (netip.Prefix, error) { return parseEgressCIDR(s) }

func parseEgressCIDR(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		if !a.Is4() {
			return netip.Prefix{}, fmt.Errorf("IPv6 is not supported")
		}
		return netip.PrefixFrom(a, 32), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("IPv6 is not supported")
	}
	return p.Masked(), nil
}

// ValidateEgress applies FluxVM's rules up front so a bad policy is refused at admission instead of
// failing the VM's creation on the node.
func ValidateEgress(n NetworkSpec) error {
	e := n.Egress
	if e == nil {
		return nil
	}
	switch e.Mode {
	case "", EgressLoopback, EgressAllowlist, EgressAudit, EgressOpen:
	default:
		return fmt.Errorf("spec.network.egress.mode %q is invalid; use loopback, allowlist, audit, or open", e.Mode)
	}
	if len(e.Rules) > MaxEgressRules {
		return fmt.Errorf("spec.network.egress.rules has %d entries; the limit is %d, use wider CIDRs", len(e.Rules), MaxEgressRules)
	}
	type key struct {
		addr   netip.Addr
		prefix int
	}
	type ports struct {
		any bool
		set map[[2]uint16]bool
	}
	own := map[key]*ports{}
	for i, r := range e.Rules {
		p, err := parseEgressCIDR(r.CIDR)
		if err != nil {
			return fmt.Errorf("spec.network.egress.rules[%d].cidr %q: %w", i, r.CIDR, err)
		}
		proto := r.Proto
		switch proto {
		case "", "any", "tcp", "udp":
		default:
			return fmt.Errorf("spec.network.egress.rules[%d].proto %q is invalid; use any, tcp, or udp", i, r.Proto)
		}
		if proto == "" {
			proto = "any"
		}
		if r.Port != 0 && proto == "any" {
			return fmt.Errorf("spec.network.egress.rules[%d]: a port needs proto tcp or udp", i)
		}
		k := key{p.Addr(), p.Bits()}
		if own[k] == nil {
			own[k] = &ports{set: map[[2]uint16]bool{}}
		}
		if r.Port == 0 {
			own[k].any = true
		} else {
			pn := uint16(6)
			if proto == "udp" {
				pn = 17
			}
			own[k].set[[2]uint16{pn, r.Port}] = true
		}
	}
	// The kernel returns only the longest matching prefix, so FluxVM folds the ports of covering
	// networks into each entry; reject here what it would reject after folding.
	keys := make([]key, 0, len(own))
	for k := range own {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].prefix != keys[j].prefix {
			return keys[i].prefix < keys[j].prefix
		}
		return keys[i].addr.Less(keys[j].addr)
	})
	for _, k := range keys {
		child := netip.PrefixFrom(k.addr, k.prefix)
		any := false
		union := map[[2]uint16]bool{}
		for o, v := range own {
			if o.prefix <= k.prefix && netip.PrefixFrom(o.addr, o.prefix).Contains(child.Addr()) {
				any = any || v.any
				for pp := range v.set {
					union[pp] = true
				}
			}
		}
		if !any && len(union) > MaxEgressPortsPerCIDR {
			return fmt.Errorf("spec.network.egress: %s would need %d port rules once rules for covering networks are included; at most %d are supported per network. Allow every port for it, or use fewer distinct ports", child, len(union), MaxEgressPortsPerCIDR)
		}
	}
	return nil
}
