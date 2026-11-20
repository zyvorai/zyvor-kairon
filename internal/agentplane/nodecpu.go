// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"github.com/zyvorai/kairon/internal/model"
)

// NodeCPUReport is what kairon-node can read from cpuset files.
type NodeCPUReport struct {
	Node         string `json:"node"`
	Effective    string `json:"effective"`
	Reserved     string `json:"reserved,omitempty"`
	CurrentLabel string `json:"currentLabel,omitempty"`
}

// LabelUpdate is a projected kairon.zyvor.dev/pinnable-cpus value.
type LabelUpdate struct {
	Node    string `json:"node"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	Changed bool   `json:"changed"`
}

func CPULabelUpdate(r NodeCPUReport) (LabelUpdate, error) {
	value, err := ProjectPinnable(r.Effective, r.Reserved)
	if err != nil {
		return LabelUpdate{}, err
	}
	return LabelUpdate{
		Node: r.Node, Key: model.PinnableCPUsLabel, Value: value, Changed: value != r.CurrentLabel,
	}, nil
}
