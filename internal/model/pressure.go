// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"time"
)

// AnnotationNodePressure is the Node annotation kairon-node writes with the
// host's measured PSI and each local Machine's own stall time (JSON
// NodePressure). DRS with -drs-signal=pressure plans from it.
const AnnotationNodePressure = "kairon.zyvor.dev/pressure"

// PSI is one sample of "some" stall time, as the percentage of wall time
// over the last 60 s (avg60) that at least one task waited on CPU, memory
// or IO. A resource the kernel does not report is 0.
type PSI struct {
	CPU    float64 `json:"cpu"`
	Memory float64 `json:"memory"`
	IO     float64 `json:"io"`
}

// Max is the worst of the three resources.
func (p PSI) Max() float64 {
	return max(p.CPU, p.Memory, p.IO)
}

// Worst names the resource with the highest stall (cpu, memory or io; cpu
// on a tie) and its value.
func (p PSI) Worst() (string, float64) {
	name, v := "cpu", p.CPU
	if p.Memory > v {
		name, v = "memory", p.Memory
	}
	if p.IO > v {
		name, v = "io", p.IO
	}
	return name, v
}

// NodePressure is AnnotationNodePressure's value. Machines is keyed by
// namespace/name and holds the stall each Machine's own cgroup reports.
type NodePressure struct {
	SampledAt string         `json:"sampledAt"`
	Host      PSI            `json:"host"`
	Machines  map[string]PSI `json:"machines,omitempty"`
}

// Encode renders p as the annotation value.
func (p NodePressure) Encode() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// Age is how long ago p was sampled; ok is false when SampledAt does not
// parse.
func (p NodePressure) Age(now time.Time) (time.Duration, bool) {
	at, err := time.Parse(time.RFC3339, p.SampledAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(at), true
}

// NodePressureOf decodes n's AnnotationNodePressure; ok is false when the
// annotation is missing or malformed.
func NodePressureOf(n Node) (NodePressure, bool) {
	raw := n.Metadata.Annotations[AnnotationNodePressure]
	if raw == "" {
		return NodePressure{}, false
	}
	var p NodePressure
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.SampledAt == "" {
		return NodePressure{}, false
	}
	return p, true
}
