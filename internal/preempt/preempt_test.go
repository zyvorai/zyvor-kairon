// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package preempt

import "testing"

func ann(policy, extra string) map[string]string {
	m := map[string]string{}
	if policy != "" {
		m[AnnPolicy] = policy
	}
	if extra != "" {
		m[AnnPreemptedBy] = extra
	}
	return m
}

func TestPlanHaltsLowestOptedInVictim(t *testing.T) {
	preemptors := []Machine{{
		Namespace: "lab", Name: "gpu", Priority: 10, Phase: "Pending",
		Annotations: map[string]string{AnnPreempt: "true"},
	}}
	candidates := []Machine{
		{Namespace: "lab", Name: "dev", Priority: 0, NodeName: "n1", PowerState: "Running", Annotations: ann("Halt", "")},
		{Namespace: "lab", Name: "batch", Priority: -5, NodeName: "n1", PowerState: "Running", Annotations: ann("Halt", "")},
		{Namespace: "lab", Name: "keep", Priority: -10, NodeName: "n1", PowerState: "Running"},
		{Namespace: "other", Name: "foreign", Priority: -10, NodeName: "n1", PowerState: "Running", Annotations: ann("Halt", "")},
		{Namespace: "lab", Name: "higher", Priority: 20, NodeName: "n1", PowerState: "Running", Annotations: ann("Halt", "")},
	}
	migrating := map[string]struct{}{"lab/batch": {}}
	got := Plan(preemptors, candidates, migrating)
	if len(got) != 1 || got[0].VictimName != "dev" {
		t.Fatalf("decision = %+v, want lab/dev (batch is migrating, keep did not opt in)", got)
	}
	if got[0].PreviousPower != "Running" || got[0].Preemptor != "lab/gpu" {
		t.Fatalf("decision = %+v", got[0])
	}
}

func TestPlanOneVictimPerPreemptorAndNoShare(t *testing.T) {
	preemptors := []Machine{
		{Namespace: "lab", Name: "a", Priority: 5, Annotations: map[string]string{AnnPreempt: "true"}},
		{Namespace: "lab", Name: "b", Priority: 9, Annotations: map[string]string{AnnPreempt: "true"}},
	}
	candidates := []Machine{
		{Namespace: "lab", Name: "only", Priority: 0, NodeName: "n1", Annotations: ann("Halt", "")},
	}
	got := Plan(preemptors, candidates, nil)
	if len(got) != 1 || got[0].Preemptor != "lab/b" {
		t.Fatalf("got %+v, want the higher preemptor to take the only victim", got)
	}
}

func TestPlanIgnoresPauseAndAlreadyPreempted(t *testing.T) {
	preemptors := []Machine{{Namespace: "lab", Name: "gpu", Priority: 10, Annotations: map[string]string{AnnPreempt: "true"}}}
	candidates := []Machine{
		{Namespace: "lab", Name: "paused-policy", Priority: 0, NodeName: "n1", Annotations: ann("Pause", "")},
		{Namespace: "lab", Name: "already", Priority: 0, NodeName: "n1", Annotations: ann("Halt", "lab/other")},
	}
	if got := Plan(preemptors, candidates, nil); len(got) != 0 {
		t.Fatalf("got %+v, want no victim", got)
	}
}

func TestResumeWhenPreemptorGoneOrHalted(t *testing.T) {
	machines := []Machine{
		{Namespace: "lab", Name: "dev", PowerState: "Halted", Annotations: map[string]string{AnnPreemptedBy: "lab/gpu", AnnPreemptedPower: "Running"}},
		{Namespace: "lab", Name: "gpu", PowerState: "Halted", NodeName: "n1"},
		{Namespace: "lab", Name: "other", PowerState: "Halted", Annotations: map[string]string{AnnPreemptedBy: "lab/missing", AnnPreemptedPower: "Running"}},
		{Namespace: "lab", Name: "held", PowerState: "Halted", Annotations: map[string]string{AnnPreemptedBy: "lab/live", AnnPreemptedPower: "Running"}},
		{Namespace: "lab", Name: "live", PowerState: "Running", NodeName: "n1", Phase: "Running"},
	}
	got := ResumePlan(machines)
	if len(got) != 2 {
		t.Fatalf("resume = %+v, want dev and other", got)
	}
	if got[0].Name != "dev" || got[1].Name != "other" {
		t.Fatalf("resume = %+v", got)
	}
}

func TestValidatePolicyRejectsPause(t *testing.T) {
	if err := ValidatePolicy(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolicy("Halt"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolicy("Pause"); err == nil {
		t.Fatal("Pause was allowed")
	}
}
