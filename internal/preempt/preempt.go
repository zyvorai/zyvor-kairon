// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Package preempt decides which already-scheduled Machine a pending
// higher-priority Machine may halt, and when that victim may resume.
//
// Both sides opt in. The pending Machine sets
// kairon.zyvor.dev/preempt=true. The victim sets
// kairon.zyvor.dev/preemption-policy=Halt. Pause is not a preemption
// action: BuildNodeLoad still counts Paused Machines, so it would not
// free the capacity the preemptor is waiting on. Halted does.
//
// One victim per preemptor per tick. The controller patches powerState
// and retries placement on the next tick, after the halt is visible.
package preempt

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// AnnPreempt on a pending Machine asks for preemption this tick.
	AnnPreempt = "kairon.zyvor.dev/preempt"
	// AnnPolicy on a running Machine allows it to be the victim.
	// Halt and Halted both mean spec.powerState=Halted.
	AnnPolicy = "kairon.zyvor.dev/preemption-policy"
	// AnnPreemptedBy records namespace/name of the Machine that halted this one.
	AnnPreemptedBy = "kairon.zyvor.dev/preempted-by"
	// AnnPreemptedPower is the powerState to restore. Empty means Running.
	AnnPreemptedPower = "kairon.zyvor.dev/preempted-power"
	// ActionHalt is the annotation value. PowerHalted is spec.powerState.
	ActionHalt  = "Halt"
	PowerHalted = "Halted"
)

// Machine is the slice of a Machine the decision needs.
type Machine struct {
	Namespace   string
	Name        string
	Tenant      string
	Priority    int32
	NodeName    string
	PowerState  string
	Phase       string
	Annotations map[string]string
	Deleting    bool
}

func (m Machine) key() string { return m.Namespace + "/" + m.Name }

// Decision is one halt the controller should patch.
type Decision struct {
	VictimNamespace string
	VictimName      string
	Preemptor       string
	PreviousPower   string
}

// Resume is one halt the controller should undo.
type Resume struct {
	Namespace string
	Name      string
	Power     string
}

// Plan picks at most one victim per unschedulable preemptor. Victims are
// not shared. migrating keys are namespace/name of Machines with a
// non-terminal MachineMigration. A victim must be same-namespace, lower
// priority, scheduled, desired Running, opted into Halt, and not migrating.
func Plan(preemptors, candidates []Machine, migrating map[string]struct{}) []Decision {
	order := append([]Machine(nil), preemptors...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Priority != order[j].Priority {
			return order[i].Priority > order[j].Priority
		}
		return order[i].key() < order[j].key()
	})
	used := map[string]struct{}{}
	var out []Decision
	for _, p := range order {
		if !wants(p) || p.Deleting || p.NodeName != "" {
			continue
		}
		v, ok := pickVictim(p, candidates, migrating, used)
		if !ok {
			continue
		}
		used[v.key()] = struct{}{}
		prev := v.PowerState
		if prev == "" {
			prev = "Running"
		}
		out = append(out, Decision{
			VictimNamespace: v.Namespace,
			VictimName:      v.Name,
			Preemptor:       p.key(),
			PreviousPower:   prev,
		})
	}
	return out
}

// ResumePlan returns halted Machines whose preemptor no longer holds
// capacity. A missing preemptor, a halted or stopped preemptor, or a
// preemptor that is no longer pending and has no node resumes the victim.
// A preemptor that is still pending, or running on a node, does not.
func ResumePlan(machines []Machine) []Resume {
	byKey := map[string]Machine{}
	for _, m := range machines {
		byKey[m.key()] = m
	}
	var out []Resume
	for _, m := range machines {
		who := strings.TrimSpace(m.Annotations[AnnPreemptedBy])
		if who == "" || m.Deleting {
			continue
		}
		if !strings.EqualFold(m.PowerState, PowerHalted) {
			continue
		}
		power := m.Annotations[AnnPreemptedPower]
		if power == "" {
			power = "Running"
		}
		if power != "Running" && power != "" {
			continue
		}
		pre, ok := byKey[who]
		if !ok || preemptorReleased(pre) {
			out = append(out, Resume{Namespace: m.Namespace, Name: m.Name, Power: "Running"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out
}

// HaltPatch sets powerState and records who halted the Machine.
func HaltPatch(d Decision) map[string]any {
	return map[string]any{
		"spec": map[string]any{"powerState": PowerHalted},
		"metadata": map[string]any{"annotations": map[string]any{
			AnnPreemptedBy:    d.Preemptor,
			AnnPreemptedPower: d.PreviousPower,
		}},
	}
}

// ResumePatch restores Running and clears the preemption annotations.
func ResumePatch(r Resume) map[string]any {
	return map[string]any{
		"spec": map[string]any{"powerState": r.Power},
		"metadata": map[string]any{"annotations": map[string]any{
			AnnPreemptedBy:    nil,
			AnnPreemptedPower: nil,
		}},
	}
}

func pickVictim(p Machine, candidates []Machine, migrating, used map[string]struct{}) (Machine, bool) {
	var best Machine
	found := false
	for _, c := range candidates {
		if !victimOK(p, c, migrating, used) {
			continue
		}
		if !found || c.Priority < best.Priority || (c.Priority == best.Priority && c.key() < best.key()) {
			best = c
			found = true
		}
	}
	return best, found
}

func victimOK(p, c Machine, migrating, used map[string]struct{}) bool {
	if c.Namespace != p.Namespace || c.Name == p.Name || c.Deleting {
		return false
	}
	if _, taken := used[c.key()]; taken {
		return false
	}
	if _, busy := migrating[c.key()]; busy {
		return false
	}
	if c.NodeName == "" || c.Priority >= p.Priority {
		return false
	}
	if !policyHalt(c.Annotations[AnnPolicy]) {
		return false
	}
	if strings.TrimSpace(c.Annotations[AnnPreemptedBy]) != "" {
		return false
	}
	desired := c.PowerState
	if desired == "" {
		desired = "Running"
	}
	return strings.EqualFold(desired, "Running")
}

func wants(m Machine) bool {
	return strings.EqualFold(strings.TrimSpace(m.Annotations[AnnPreempt]), "true")
}

func preemptorReleased(p Machine) bool {
	if p.Deleting {
		return true
	}
	desired := p.PowerState
	if desired == "" {
		desired = "Running"
	}
	if strings.EqualFold(desired, PowerHalted) || strings.EqualFold(desired, "Stopped") {
		return true
	}
	if p.NodeName == "" && !strings.EqualFold(p.Phase, "Pending") {
		return true
	}
	return false
}

// ValidatePolicy rejects a preemption-policy value the controller will
// ignore. Empty is allowed. Pause is rejected so an operator does not
// think it frees capacity.
func ValidatePolicy(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "halt", "halted":
		return nil
	default:
		return fmt.Errorf("%s must be Halt or empty, got %q (Pause does not free node capacity)", AnnPolicy, v)
	}
}

func policyHalt(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "halt", "halted":
		return true
	default:
		return false
	}
}
