// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zyvorai/kairon/internal/llm"
)

// DiagnoseFacts is what the caller read from the cluster for one object.
type DiagnoseFacts struct {
	Kind       string          `json:"kind"`
	Namespace  string          `json:"namespace,omitempty"`
	Name       string          `json:"name"`
	Phase      string          `json:"phase,omitempty"`
	Message    string          `json:"message,omitempty"`
	Conditions []FactCondition `json:"conditions,omitempty"`
	Events     []FactEvent     `json:"events,omitempty"`
	Nodes      []NodeFit       `json:"nodes,omitempty"`
	Drops      []Drop          `json:"drops,omitempty"`
	Boot       []BootFinding   `json:"boot,omitempty"`
}

type FactCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type FactEvent struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

// Cause is one ranked explanation. Propose lists operator actions; none
// is applied.
type Cause struct {
	Rank     int      `json:"rank"`
	Title    string   `json:"title"`
	Evidence string   `json:"evidence,omitempty"`
	Propose  []string `json:"propose,omitempty"`
}

type Diagnosis struct {
	Subject   string       `json:"subject"`
	Healthy   bool         `json:"healthy"`
	Summary   string       `json:"summary"`
	Causes    []Cause      `json:"causes,omitempty"`
	Repair    []RepairStep `json:"repair,omitempty"`
	AISummary string       `json:"aiSummary,omitempty"`
	Apply     bool         `json:"apply"`
}

// cause weights; lower ranks first.
const (
	weightPhase = iota
	weightCondition
	weightEvent
	weightDrop
	weightBoot
)

type weighted struct {
	w     int
	count int
	c     Cause
}

// Diagnose ranks causes from facts without a model: failed phase, false
// conditions, repeated Warning events, drops and boot findings, each
// with the compiled proposal for it.
func Diagnose(f DiagnoseFacts) Diagnosis {
	subject := strings.ToLower(f.Kind) + "/" + f.Name
	if f.Namespace != "" {
		subject = f.Namespace + "/" + subject
	}
	var ws []weighted
	if p := strings.ToLower(f.Phase); p == "failed" || p == "error" || p == "blocked" || p == "needsrecovery" || p == "lost" {
		ws = append(ws, weighted{w: weightPhase, c: Cause{Title: fmt.Sprintf("%s is %s", f.Kind, f.Phase), Evidence: f.Message, Propose: phaseProposals(f.Kind, f.Phase)}})
	}
	for _, c := range f.Conditions {
		if !conditionBad(c) {
			continue
		}
		ws = append(ws, weighted{w: weightCondition, c: Cause{
			Title:    fmt.Sprintf("condition %s is %s (%s)", c.Type, c.Status, c.Reason),
			Evidence: c.Message,
			Propose:  conditionProposals(c, f),
		}})
	}
	byReason := map[string]*weighted{}
	for _, e := range f.Events {
		if !strings.EqualFold(e.Type, "Warning") {
			continue
		}
		n := max(e.Count, 1)
		if w, ok := byReason[e.Reason]; ok {
			w.count += n
			continue
		}
		byReason[e.Reason] = &weighted{w: weightEvent, count: n, c: Cause{Title: "Warning events: " + e.Reason, Evidence: e.Message, Propose: eventProposals(e)}}
	}
	for _, w := range byReason {
		w.c.Title = fmt.Sprintf("%s (x%d)", w.c.Title, w.count)
		ws = append(ws, *w)
	}
	for _, h := range ExplainDrops(f.Drops) {
		ws = append(ws, weighted{w: weightDrop, count: 1, c: Cause{Title: "edge drop: " + h.Reason, Evidence: h.Summary + evidenceSuffix(h.Evidence), Propose: h.Propose}})
	}
	d := Diagnosis{Subject: subject, Repair: ProposeRepair(f.Boot)}
	if len(f.Boot) > 0 {
		ws = append(ws, weighted{w: weightBoot, c: Cause{Title: fmt.Sprintf("%d boot finding(s) on the imported disk", len(f.Boot)), Propose: []string{"review the repair steps; none is applied"}}})
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].w != ws[j].w {
			return ws[i].w < ws[j].w
		}
		return ws[i].count > ws[j].count
	})
	for i, w := range ws {
		w.c.Rank = i + 1
		d.Causes = append(d.Causes, w.c)
	}
	if len(d.Causes) == 0 {
		d.Healthy = true
		d.Summary = fmt.Sprintf("%s shows no failed phase, false condition, Warning event or drop", subject)
	} else {
		d.Summary = fmt.Sprintf("%s: %d cause(s); most likely: %s", subject, len(d.Causes), d.Causes[0].Title)
	}
	return d
}

func evidenceSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " (" + e + ")"
}

// conditionBad reads polarity from the type: Ready-style conditions are
// bad when False, problem-style ones (NodeUnreachable, Fenced,
// MemoryPressure) when True. Paused and Migrating are states, not
// problems.
func conditionBad(c FactCondition) bool {
	switch c.Type {
	case "Paused", "Migrating":
		return false
	case "NodeUnreachable", "Fenced", "Degraded", "Unschedulable":
		return strings.EqualFold(c.Status, "True")
	}
	for _, suffix := range []string{"Pressure", "Unreachable", "Unavailable", "Failed", "Failure"} {
		if strings.HasSuffix(c.Type, suffix) {
			return strings.EqualFold(c.Status, "True")
		}
	}
	return strings.EqualFold(c.Status, "False")
}

func phaseProposals(kind, phase string) []string {
	switch strings.ToLower(phase) {
	case "needsrecovery":
		return []string{"read status.recovery for the source/target diagnosis", "recover only after confirming which side holds the live guest; Kairon will not guess"}
	case "blocked":
		return []string{"read status.message: a disruption budget, concurrency cap or ineligible target blocks it", "fix the blocker, then recreate the migration"}
	case "lost":
		return []string{"the bound Machine is gone; release the claim and claim again"}
	}
	if strings.EqualFold(kind, "migration") {
		return []string{"read status.message and the node agent logs on source and target", "retry with strategy cold if live migration keeps failing"}
	}
	return []string{"read status.message and kairon-node logs on the assigned node", "check the boot image path or source digest"}
}

func conditionProposals(c FactCondition, f DiagnoseFacts) []string {
	switch c.Reason {
	case "Unschedulable":
		ex := ExplainPending(f.Name, f.Nodes)
		return append([]string{ex.Summary}, ex.Hints...)
	case "QuotaBlocked":
		return []string{"raise the MachineQuota or stop another Machine in this namespace"}
	case "CPUPinningFailed":
		return []string{"no node has enough pinnable CPUs; lower CPU or disable pinning"}
	}
	return []string{"read the condition message; do not clear the constraint to force progress"}
}

func eventProposals(e FactEvent) []string {
	switch e.Reason {
	case "EdgeDrop":
		return []string{"run kaironctl agent explain-drops on the Machine's attributed drops before widening policy"}
	case "EdgeAnomaly":
		return []string{"inspect the guest for the flagged destination; isolate the Machine if the beacon is unexpected"}
	case "QuotaBlocked":
		return []string{"raise the MachineQuota or stop another Machine"}
	case "NodeUnreachable":
		return []string{"check the node; cordon-evacuate moves Machines off it if enabled"}
	}
	return []string{"read the event message"}
}

var diagnoseSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"summary": map[string]any{"type": "string"}},
	"required":   []string{"summary"},
}

// Summarize adds a short model-written summary of an existing diagnosis.
// The causes and proposals stay the deterministic ones.
func Summarize(ctx context.Context, chat Chatter, d Diagnosis) (Diagnosis, error) {
	if chat == nil {
		return d, llm.ErrNotConfigured
	}
	b, _ := json.Marshal(d)
	raw, err := chat.ChatJSON(ctx, []llm.Message{
		{Role: "system", Content: `You summarize a Kairon incident diagnosis for an on-call operator in at most three sentences. Use only the causes given. Do not invent facts or commands. Answer as {"summary": "..."}.`},
		{Role: "user", Content: "DIAGNOSIS:\n" + string(b)},
	}, "kairon_incident_summary", diagnoseSchema)
	if err != nil {
		return d, err
	}
	var out struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || strings.TrimSpace(out.Summary) == "" {
		return d, fmt.Errorf("model summary was not {\"summary\": ...}")
	}
	d.AISummary = strings.TrimSpace(out.Summary)
	return d, nil
}
