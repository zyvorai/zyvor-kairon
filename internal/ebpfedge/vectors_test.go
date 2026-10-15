// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package ebpfedge

import (
	"encoding/json"
	"os"
	"testing"
)

type vectorCase struct {
	Name   string     `json:"name"`
	Spec   EdgeSpec   `json:"spec"`
	Packet PacketMeta `json:"packet"`
	Action string     `json:"action"`
	Reason string     `json:"reason"`
	XDP    bool       `json:"xdp"`
}

// TestDecisionVectors pins Decide to testdata/vectors.json. The same file is
// the contract the FluxVM edge program is checked against, so a change to a
// decision here must change the vectors in the same commit.
func TestDecisionVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []vectorCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range doc.Cases {
		spec, err := Compile(c.Spec)
		if err != nil {
			t.Errorf("%s: compile: %v", c.Name, err)
			continue
		}
		d := Decide(spec, c.Packet)
		if d.Action != c.Action || d.Reason != c.Reason || d.XDP != c.XDP {
			t.Errorf("%s: got {%s %s xdp=%v}, want {%s %s xdp=%v}", c.Name, d.Action, d.Reason, d.XDP, c.Action, c.Reason, c.XDP)
		}
	}
}
