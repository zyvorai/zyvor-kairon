// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// ClusterEvent is the Kubernetes Event a controller should emit.
// Type is Normal or Warning. Involved is the Machine name.
type ClusterEvent struct {
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Involved string `json:"involved"`
}

func EventsFromFindings(machine string, findings []Finding) []ClusterEvent {
	out := make([]ClusterEvent, 0, len(findings))
	for _, f := range findings {
		out = append(out, ClusterEvent{
			Type:     "Warning",
			Reason:   "EdgeAnomaly",
			Message:  fmt.Sprintf("%s: %s", f.Kind, f.Summary),
			Involved: machine,
		})
	}
	return out
}

func EventsFromDrops(machine string, drops []Drop) []ClusterEvent {
	out := make([]ClusterEvent, 0)
	for _, h := range ExplainDrops(drops) {
		if h.Reason == "spoof_ip" || h.Reason == "spoof_mac" || h.Reason == "dns_deny" || h.Reason == "sni_deny" {
			out = append(out, ClusterEvent{
				Type: "Warning", Reason: "EdgeDrop", Message: h.Reason + ": " + h.Summary, Involved: machine,
			})
		}
	}
	return out
}
