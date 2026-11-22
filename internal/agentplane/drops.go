// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// Drop is one attributed edge drop. Reason matches FluxVM's map
// (spoof_ip, dns_deny, sni_deny, rate_limit, ...).
type Drop struct {
	Reason  string `json:"reason"`
	Dst     string `json:"dst,omitempty"`
	SNI     string `json:"sni,omitempty"`
	DNS     string `json:"dns,omitempty"`
	Count   int    `json:"count,omitempty"`
	Process string `json:"process,omitempty"`
}

// Hypothesis is a proposal. Apply is always false.
type Hypothesis struct {
	Reason   string   `json:"reason"`
	Summary  string   `json:"summary"`
	Evidence string   `json:"evidence,omitempty"`
	Propose  []string `json:"propose,omitempty"`
	Apply    bool     `json:"apply"`
}

func ExplainDrops(drops []Drop) []Hypothesis {
	out := make([]Hypothesis, 0, len(drops))
	for _, d := range drops {
		h := Hypothesis{Reason: d.Reason, Apply: false}
		if d.Count > 0 {
			h.Evidence = fmt.Sprintf("count=%d", d.Count)
		}
		if d.Process != "" {
			if h.Evidence != "" {
				h.Evidence += " "
			}
			h.Evidence += "process=" + d.Process
		}
		switch d.Reason {
		case "spoof_ip", "spoof_mac":
			h.Summary = "guest source address does not match the address assigned to this Machine"
			h.Propose = []string{"keep antiSpoof enabled", "do not add an allowlist entry for a spoofed source"}
		case "dns_deny":
			h.Summary = "DNS qname is outside the MachineNetworkPolicy allowlist"
			if d.DNS != "" {
				h.Propose = []string{"if this name is intended, add it to allowFqdns: " + d.DNS}
			} else {
				h.Propose = []string{"inspect the qname before adding an allowFqdns entry"}
			}
		case "sni_deny":
			h.Summary = "TLS SNI is outside the allowlist"
			if d.SNI != "" {
				h.Propose = []string{"if this server is intended, add it to allowSNI: " + d.SNI}
			} else {
				h.Propose = []string{"capture one flow before widening allowSNI"}
			}
		case "rate_limit":
			h.Summary = "edge token bucket dropped the frame"
			h.Propose = []string{"raise maxEgressMbps only if the workload is expected to burst"}
		default:
			h.Summary = "drop reason has no compiled explanation; treat it as deny"
			h.Propose = []string{"read network-effective before changing policy"}
		}
		out = append(out, h)
	}
	return out
}
