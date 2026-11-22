// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// Flow is a coarse edge sample. Detection is rule-based so the
// dataplane stays deterministic when this package is off.
type Flow struct {
	DstIP       string `json:"dstIP,omitempty"`
	SNI         string `json:"sni,omitempty"`
	DNS         string `json:"dns,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	IntervalSec int    `json:"intervalSec,omitempty"`
}

type Finding struct {
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	Apply   bool   `json:"apply"`
}

func Detect(flows []Flow, drops []Drop) []Finding {
	var out []Finding
	byDst := map[string][]Flow{}
	sni := map[string]struct{}{}
	for _, f := range flows {
		if f.DstIP != "" {
			byDst[f.DstIP] = append(byDst[f.DstIP], f)
		}
		if f.SNI != "" {
			sni[f.SNI] = struct{}{}
		}
		if len(f.DNS) > 80 {
			out = append(out, Finding{Kind: "dns_tunnel_shape", Summary: "DNS qname longer than 80 bytes: " + trim(f.DNS, 40), Apply: false})
		}
	}
	for dst, group := range byDst {
		if len(group) < 3 {
			continue
		}
		regular := true
		var prev int
		for i, g := range group {
			if g.Bytes > 512 || g.IntervalSec <= 0 {
				regular = false
				break
			}
			if i > 0 && abs(g.IntervalSec-prev) > 2 {
				regular = false
				break
			}
			prev = g.IntervalSec
		}
		if regular {
			out = append(out, Finding{Kind: "beacon", Summary: fmt.Sprintf("%d small regular flows to %s", len(group), dst), Apply: false})
		}
	}
	if len(sni) >= 8 {
		out = append(out, Finding{Kind: "sni_spread", Summary: fmt.Sprintf("%d distinct SNI values in one sample", len(sni)), Apply: false})
	}
	denies := 0
	for _, d := range drops {
		if d.Reason == "dns_deny" || d.Reason == "sni_deny" {
			denies += max(d.Count, 1)
		}
	}
	if denies >= 20 {
		out = append(out, Finding{Kind: "deny_burst", Summary: fmt.Sprintf("%d dns/sni denies in sample", denies), Apply: false})
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
