// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package scheduler

import (
	"hash/fnv"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func node(name string, ready, capable bool) model.Node {
	var n model.Node
	n.Metadata.Name = name
	n.Metadata.Labels = map[string]string{"kubernetes.io/arch": "amd64"}
	if capable {
		n.Metadata.Labels[model.CapableLabel] = "true"
	}
	status := "False"
	if ready {
		status = "True"
	}
	n.Status.Conditions = []model.NodeCondition{{Type: "Ready", Status: status}}
	return n
}

func TestChooseLeastLoaded(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "prod"}}
	s := Scheduler{RequireCapableLabel: true}
	got, err := s.Choose(m, []model.Node{node("a", true, true), node("b", true, true)}, nil, LoadFromCounts(map[string]int{"a": 5, "b": 1}), "")
	if err != nil || got != "b" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestChooseRejectsUnready(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	s := Scheduler{RequireCapableLabel: true}
	_, err := s.Choose(m, []model.Node{node("a", false, true)}, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil {
		t.Fatal("expected no eligible nodes")
	}
}

func TestChooseExcludesUnschedulable(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	s := Scheduler{RequireCapableLabel: true}
	n := node("a", true, true)
	n.Spec.Unschedulable = true
	_, err := s.Choose(m, []model.Node{n}, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil {
		t.Fatal("expected unschedulable node to be excluded")
	}
}

func TestChooseAllowsUncapableWhenLabelNotRequired(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	s := Scheduler{RequireCapableLabel: false}
	got, err := s.Choose(m, []model.Node{node("a", true, false)}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "a" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestChooseFiltersByArchitecture(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	arm := node("arm-node", true, true)
	arm.Metadata.Labels["kubernetes.io/arch"] = "arm64"
	amd := node("amd-node", true, true)

	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}, Spec: model.MachineSpec{Placement: model.PlacementSpec{Architecture: "arm64"}}}
	got, err := s.Choose(m, []model.Node{arm, amd}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "arm-node" {
		t.Fatalf("got %q err=%v", got, err)
	}

	noMatch := model.Machine{Metadata: model.ObjectMeta{Name: "db"}, Spec: model.MachineSpec{Placement: model.PlacementSpec{Architecture: "riscv64"}}}
	if _, err := s.Choose(noMatch, []model.Node{arm, amd}, nil, LoadFromCounts(map[string]int{}), ""); err == nil {
		t.Fatal("expected no node to match an unavailable architecture")
	}
}

func TestChooseFiltersByNodeSelector(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"

	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}, Spec: model.MachineSpec{Placement: model.PlacementSpec{NodeSelector: map[string]string{"zone": "us-east"}}}}
	got, err := s.Choose(m, []model.Node{east, west}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "east" {
		t.Fatalf("got %q err=%v", got, err)
	}

	noMatch := model.Machine{Metadata: model.ObjectMeta{Name: "db"}, Spec: model.MachineSpec{Placement: model.PlacementSpec{NodeSelector: map[string]string{"zone": "us-central"}}}}
	if _, err := s.Choose(noMatch, []model.Node{east, west}, nil, LoadFromCounts(map[string]int{}), ""); err == nil {
		t.Fatal("expected no node to match an unsatisfied node selector")
	}
}

func TestChooseTieBreaksByHashAcrossEqualLoad(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	nodes := []model.Node{node("a", true, true), node("b", true, true), node("c", true, true)}
	assigned := map[string]int{"a": 0, "b": 0, "c": 0}

	expected := func(ns string) string {
		h := fnv.New32a()
		_, _ = h.Write([]byte("default/" + ns))
		return []string{"a", "b", "c"}[int(h.Sum32())%3]
	}

	for _, name := range []string{"machine-one", "machine-two", "machine-three"} {
		m := model.Machine{Metadata: model.ObjectMeta{Name: name}}
		got, err := s.Choose(m, nodes, nil, LoadFromCounts(assigned), "")
		if err != nil {
			t.Fatalf("machine=%s err=%v", name, err)
		}
		if got != expected(name) {
			t.Fatalf("machine=%s got=%q want=%q", name, got, expected(name))
		}
		// Determinism: calling again for the same machine must return the same node.
		again, err := s.Choose(m, nodes, nil, LoadFromCounts(assigned), "")
		if err != nil || again != got {
			t.Fatalf("machine=%s not deterministic: first=%q second=%q err=%v", name, got, again, err)
		}
	}
}

func TestChooseAffinityAndAntiAffinity(t *testing.T) {
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"
	other := model.Machine{
		Metadata: model.ObjectMeta{Name: "other", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec:     model.MachineSpec{NodeName: "east"},
	}
	term := model.MachineAffinityTerm{LabelSelector: map[string]string{"tier": "web"}, TopologyKey: "zone"}

	cases := []struct {
		name     string
		nodes    []model.Node
		machines []model.Machine
		spec     model.PlacementSpec
		want     string
		wantErr  bool
	}{
		{"affinity co-locates with a matching machine", []model.Node{east, west}, []model.Machine{other}, model.PlacementSpec{Affinity: []model.MachineAffinityTerm{term}}, "east", false},
		{"affinity rejects when nothing matches", []model.Node{east}, nil, model.PlacementSpec{Affinity: []model.MachineAffinityTerm{term}}, "", true},
		{"anti-affinity excludes the shared-zone node", []model.Node{east, west}, []model.Machine{other}, model.PlacementSpec{AntiAffinity: []model.MachineAffinityTerm{term}}, "west", false},
		{"anti-affinity allows when nothing matches", []model.Node{east}, nil, model.PlacementSpec{AntiAffinity: []model.MachineAffinityTerm{term}}, "east", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Scheduler{RequireCapableLabel: true}
			m := model.Machine{Metadata: model.ObjectMeta{Name: "subject", Namespace: "default"}, Spec: model.MachineSpec{Placement: tc.spec}}
			got, err := s.Choose(m, tc.nodes, tc.machines, LoadFromCounts(map[string]int{}), "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestChooseAntiAffinityIgnoresTheMachineBeingScheduledItself(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"

	// A Machine being re-evaluated (e.g. as a migration target search) that
	// already carries the anti-affinity selector's own labels must not
	// exclude itself.
	self := model.Machine{
		Metadata: model.ObjectMeta{Name: "solo", Namespace: "default", Labels: map[string]string{"role": "db-primary"}},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{
			AntiAffinity: []model.MachineAffinityTerm{{LabelSelector: map[string]string{"role": "db-primary"}, TopologyKey: "zone"}},
		}},
	}
	got, err := s.Choose(self, []model.Node{east}, []model.Machine{self}, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "east" {
		t.Fatalf("got %q err=%v, want east (must not self-exclude)", got, err)
	}
}

func TestChooseNoEligibleNodesReasonIsPlacement(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	nodes := []model.Node{node("a", true, true), node("b", true, true)}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}, Spec: model.MachineSpec{Placement: model.PlacementSpec{Architecture: "riscv64"}}}
	_, err := s.Choose(m, nodes, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil {
		t.Fatal("expected an error when Ready/capable nodes exist but none match placement")
	}
}

func TestChoosePreferredAffinityScoring(t *testing.T) {
	term := model.MachineAffinityTerm{LabelSelector: map[string]string{"tier": "web"}, TopologyKey: "zone"}
	other := model.Machine{
		Metadata: model.ObjectMeta{Name: "other", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec:     model.MachineSpec{NodeName: "east"},
	}

	cases := []struct {
		name     string
		spec     model.PlacementSpec
		assigned map[string]int
		want     string
	}{
		{
			"a strong preferred affinity outweighs a 5-machine load imbalance",
			model.PlacementSpec{PreferredAffinity: []model.WeightedAffinityTerm{{Weight: 20, MachineAffinityTerm: term}}},
			map[string]int{"east": 5, "west": 0},
			"east",
		},
		{
			"a weight-1 preferred affinity doesn't survive a 5-machine load gap",
			model.PlacementSpec{PreferredAffinity: []model.WeightedAffinityTerm{{Weight: 1, MachineAffinityTerm: term}}},
			map[string]int{"east": 5, "west": 0},
			"west",
		},
		{
			"preferred anti-affinity alone steers away from the matching node at equal load",
			model.PlacementSpec{PreferredAntiAffinity: []model.WeightedAffinityTerm{{Weight: 20, MachineAffinityTerm: term}}},
			map[string]int{"east": 0, "west": 0},
			"west",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Scheduler{RequireCapableLabel: true}
			east := node("east", true, true)
			east.Metadata.Labels["zone"] = "us-east"
			west := node("west", true, true)
			west.Metadata.Labels["zone"] = "us-west"
			m := model.Machine{Metadata: model.ObjectMeta{Name: "subject", Namespace: "default"}, Spec: model.MachineSpec{Placement: tc.spec}}
			got, err := s.Choose(m, []model.Node{east, west}, []model.Machine{other}, LoadFromCounts(tc.assigned), "")
			if err != nil || got != tc.want {
				t.Fatalf("got %q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}

func TestChooseTopologySpreadPrefersTheEmptierDomain(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"
	existing := []model.Machine{
		{Metadata: model.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "east"}},
		{Metadata: model.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "east"}},
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-3", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{
			TopologySpreadConstraints: []model.TopologySpreadConstraint{{
				TopologyKey:   "zone",
				LabelSelector: map[string]string{"tier": "web"},
			}},
		}},
	}
	// Equal load (0 assigned each): topology spread alone should favor west,
	// which has zero existing "tier=web" machines vs east's two.
	got, err := s.Choose(m, []model.Node{east, west}, existing, LoadFromCounts(map[string]int{"east": 0, "west": 0}), "")
	if err != nil || got != "west" {
		t.Fatalf("got %q err=%v, want west (fewer existing tier=web machines in that topology domain)", got, err)
	}
}

func TestChooseHardMaxSkewRejectsNodeEvenWhenScoringWouldPreferIt(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"
	existing := []model.Machine{
		{Metadata: model.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "east"}},
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{
			TopologySpreadConstraints: []model.TopologySpreadConstraint{{
				TopologyKey:       "zone",
				LabelSelector:     map[string]string{"tier": "web"},
				MaxSkew:           0,
				WhenUnsatisfiable: model.WhenUnsatisfiableDoNotSchedule,
			}},
		}},
	}
	// west is massively overloaded (100 unrelated Machines), so pure
	// load-balancing scoring would strongly prefer east -- but east
	// already has 1 tier=web Machine and west has 0; placing web-2 on
	// east would make east=2 vs west's unchanged 0, skew 2 > maxSkew 0.
	// Placing on west instead makes it 1 vs east's 1, skew 0 -- allowed.
	// DoNotSchedule must reject east outright despite its far better
	// load score.
	got, err := s.Choose(m, []model.Node{east, west}, existing, LoadFromCounts(map[string]int{"east": 0, "west": 100}), "")
	if err != nil || got != "west" {
		t.Fatalf("got %q err=%v, want west (east hard-rejected by maxSkew despite a much better load score)", got, err)
	}
}

func TestChooseHardMaxSkewReturnsErrorWhenNoNodeSatisfiesIt(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"
	existing := []model.Machine{
		{Metadata: model.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "east"}},
		{Metadata: model.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "west"}},
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-3", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{
			TopologySpreadConstraints: []model.TopologySpreadConstraint{{
				TopologyKey:       "zone",
				LabelSelector:     map[string]string{"tier": "web"},
				MaxSkew:           0,
				WhenUnsatisfiable: model.WhenUnsatisfiableDoNotSchedule,
			}},
		}},
	}
	// Both domains already balanced at 1 each -- placing the third
	// Machine in either one makes it 2 vs the other's unchanged 1, skew 1
	// > maxSkew 0. No node can satisfy it.
	_, err := s.Choose(m, []model.Node{east, west}, existing, LoadFromCounts(map[string]int{"east": 0, "west": 0}), "")
	if err == nil {
		t.Fatal("expected an error: no node can satisfy maxSkew 0 when both domains are already balanced")
	}
}

func TestChooseScheduleAnywayNeverHardRejects(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	east := node("east", true, true)
	east.Metadata.Labels["zone"] = "us-east"
	west := node("west", true, true)
	west.Metadata.Labels["zone"] = "us-west"
	existing := []model.Machine{
		{Metadata: model.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{"tier": "web"}}, Spec: model.MachineSpec{NodeName: "east"}},
	}
	// Explicit ScheduleAnyway (the default, tested here spelled out) --
	// same skew-violating setup as the hard-rejection test above, but
	// this must never error, and east must still be a legal candidate.
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-2", Namespace: "default", Labels: map[string]string{"tier": "web"}},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{
			TopologySpreadConstraints: []model.TopologySpreadConstraint{{
				TopologyKey:       "zone",
				LabelSelector:     map[string]string{"tier": "web"},
				MaxSkew:           0,
				WhenUnsatisfiable: model.WhenUnsatisfiableScheduleAnyway,
			}},
		}},
	}
	got, err := s.Choose(m, []model.Node{east, west}, existing, LoadFromCounts(map[string]int{"east": 0, "west": 0}), "")
	if err != nil {
		t.Fatalf("ScheduleAnyway must never return an error: %v", err)
	}
	if got != "west" {
		t.Fatalf("got %q, want west (still the soft-scoring winner via topologySpreadPenalty, just not a hard requirement)", got)
	}
}

func TestChooseDRAPreferredNodeBreaksTies(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	a := node("a", true, true)
	b := node("b", true, true)
	m := model.Machine{Metadata: model.ObjectMeta{Name: "gpu-vm", Namespace: "default"}}
	got, err := s.Choose(m, []model.Node{a, b}, nil, LoadFromCounts(map[string]int{"a": 0, "b": 0}), "b")
	if err != nil || got != "b" {
		t.Fatalf("got %q err=%v, want b (the DRA-preferred node)", got, err)
	}
}

func TestChooseWithNoSoftSignalsIgnoresUnrelatedDRAHint(t *testing.T) {
	// A hint naming a node that isn't even in the candidate set must not
	// break anything -- it just never matches.
	s := Scheduler{RequireCapableLabel: true}
	a := node("a", true, true)
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db", Namespace: "default"}}
	got, err := s.Choose(m, []model.Node{a}, nil, LoadFromCounts(map[string]int{"a": 0}), "some-other-node")
	if err != nil || got != "a" {
		t.Fatalf("got %q err=%v, want a", got, err)
	}
}

func TestChooseNamesTheBlockingConstraintWhenEveryNodeIsFiltered(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "default"},
		Spec:     model.MachineSpec{Placement: model.PlacementSpec{NodeSelector: map[string]string{"zone": "us-east"}}},
	}
	a := node("a", true, true)
	b := node("b", true, true)
	_, err := s.Choose(m, []model.Node{a, b}, nil, LoadFromCounts(map[string]int{"a": 0, "b": 0}), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "2 node(s): nodeSelector zone=us-east not satisfied") {
		t.Fatalf("error doesn't name the blocking constraint: %v", err)
	}
}

func TestChooseAggregatesDistinctBlockingReasonsAcrossNodes(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "db", Namespace: "default"},
		Spec:     model.MachineSpec{Placement: model.PlacementSpec{Architecture: "arm64"}},
	}
	amd := node("amd-node", true, true)
	notReady := node("down-node", false, true)
	_, err := s.Choose(m, []model.Node{amd, notReady}, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `1 node(s): requires architecture "arm64"`) {
		t.Fatalf("error doesn't name the architecture mismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "1 node(s): node is unschedulable or not Ready") {
		t.Fatalf("error doesn't name the not-Ready node separately: %v", err)
	}
}

func TestChooseNamesInsufficientPinnableCPUs(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "pinned", Namespace: "default"},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "8", CPUPinning: true}},
	}
	a := node("a", true, true)
	a.Metadata.Labels[model.PinnableCPUsLabel] = "2-5"
	_, err := s.Choose(m, []model.Node{a}, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cpuPinning requests 8 vCPU(s), only 4 free") {
		t.Fatalf("error doesn't name the cpuPinning shortfall: %v", err)
	}
}

func TestChooseNoScheduleTaintExcludesUntoleratedMachine(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	tainted := node("tainted", true, true)
	tainted.Spec.Taints = []model.Taint{{Key: "dedicated", Value: "gpu", Effect: model.TaintEffectNoSchedule}}
	clean := node("clean", true, true)

	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	got, err := s.Choose(m, []model.Node{tainted, clean}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "clean" {
		t.Fatalf("expected the untainted node, got %q err=%v", got, err)
	}

	// Alone, the tainted node is entirely unschedulable for a Machine with no toleration.
	_, err = s.Choose(m, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), "")
	if err == nil || !strings.Contains(err.Error(), "untolerated NoSchedule taint dedicated=gpu") {
		t.Fatalf("expected an error naming the untolerated taint, got %v", err)
	}
}

func TestChooseNoExecuteTaintExcludesUntoleratedMachine(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	tainted := node("tainted", true, true)
	tainted.Spec.Taints = []model.Taint{{Key: "node.kubernetes.io/unreachable", Effect: model.TaintEffectNoExecute}}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	if _, err := s.Choose(m, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), ""); err == nil {
		t.Fatal("expected a NoExecute taint to exclude the node same as NoSchedule")
	}
}

func TestChooseEqualTolerationAdmitsMatchingTaint(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	tainted := node("tainted", true, true)
	tainted.Spec.Taints = []model.Taint{{Key: "dedicated", Value: "gpu", Effect: model.TaintEffectNoSchedule}}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "gpu-job"},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{Tolerations: []model.Toleration{
			{Key: "dedicated", Operator: model.TolerationOpEqual, Value: "gpu", Effect: model.TaintEffectNoSchedule},
		}}},
	}
	got, err := s.Choose(m, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "tainted" {
		t.Fatalf("expected the toleration to admit the tainted node, got %q err=%v", got, err)
	}

	// A different value doesn't match Equal.
	wrongValue := model.Machine{
		Metadata: model.ObjectMeta{Name: "gpu-job"},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{Tolerations: []model.Toleration{
			{Key: "dedicated", Operator: model.TolerationOpEqual, Value: "tpu"},
		}}},
	}
	if _, err := s.Choose(wrongValue, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), ""); err == nil {
		t.Fatal("expected a mismatched toleration value to still exclude the node")
	}
}

func TestChooseExistsTolerationIgnoresValue(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	tainted := node("tainted", true, true)
	tainted.Spec.Taints = []model.Taint{{Key: "dedicated", Value: "gpu", Effect: model.TaintEffectNoSchedule}}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "job"},
		Spec: model.MachineSpec{Placement: model.PlacementSpec{Tolerations: []model.Toleration{
			{Key: "dedicated", Operator: model.TolerationOpExists},
		}}},
	}
	got, err := s.Choose(m, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "tainted" {
		t.Fatalf("expected Exists to tolerate regardless of value, got %q err=%v", got, err)
	}
}

func TestChooseEmptyKeyExistsTolerationsToleratesEverything(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	tainted := node("tainted", true, true)
	tainted.Spec.Taints = []model.Taint{
		{Key: "dedicated", Value: "gpu", Effect: model.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/unreachable", Effect: model.TaintEffectNoExecute},
	}
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "job"},
		Spec:     model.MachineSpec{Placement: model.PlacementSpec{Tolerations: []model.Toleration{{Operator: model.TolerationOpExists}}}},
	}
	got, err := s.Choose(m, []model.Node{tainted}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "tainted" {
		t.Fatalf("expected the wildcard toleration to admit every taint, got %q err=%v", got, err)
	}
}

func TestChoosePreferNoScheduleOnlyPenalizesScoreNeverExcludes(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	preferAvoid := node("preferAvoid", true, true)
	preferAvoid.Spec.Taints = []model.Taint{{Key: "maintenance", Effect: model.TaintEffectPreferNoSchedule}}
	clean := node("clean", true, true)

	m := model.Machine{Metadata: model.ObjectMeta{Name: "db"}}
	got, err := s.Choose(m, []model.Node{preferAvoid, clean}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "clean" {
		t.Fatalf("expected PreferNoSchedule to steer toward the clean node, got %q err=%v", got, err)
	}

	// Alone, a PreferNoSchedule taint never makes the node ineligible.
	got, err = s.Choose(m, []model.Node{preferAvoid}, nil, LoadFromCounts(map[string]int{}), "")
	if err != nil || got != "preferAvoid" {
		t.Fatalf("expected a PreferNoSchedule-only node to still be schedulable, got %q err=%v", got, err)
	}
}

func namedMachine(name string, priority int32) model.Machine {
	return model.Machine{
		Metadata: model.ObjectMeta{Name: name, Namespace: "default"},
		Spec:     model.MachineSpec{Priority: priority},
	}
}

func machineNames(machines []model.Machine) []string {
	names := make([]string, len(machines))
	for i, m := range machines {
		names[i] = m.Metadata.Name
	}
	return names
}

func TestSortByPriorityDescHighestFirst(t *testing.T) {
	pending := []model.Machine{namedMachine("low", 0), namedMachine("high", 10), namedMachine("mid", 5)}
	SortByPriorityDesc(pending)
	got := machineNames(pending)
	want := []string{"high", "mid", "low"}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSortByPriorityDescNegativePrioritySortsBelowZero(t *testing.T) {
	pending := []model.Machine{namedMachine("below", -5), namedMachine("default", 0)}
	SortByPriorityDesc(pending)
	if got := machineNames(pending); got[0] != "default" || got[1] != "below" {
		t.Fatalf("got %v, want [default below]", got)
	}
}

// TestSortByPriorityDescStableOnTies confirms every Machine before this
// field existed -- Priority always 0 -- schedules in exactly the arrival
// order it always did: SortByPriorityDesc must never reorder within a
// shared Priority value.
func TestSortByPriorityDescStableOnTies(t *testing.T) {
	pending := []model.Machine{namedMachine("c", 0), namedMachine("a", 0), namedMachine("b", 0)}
	SortByPriorityDesc(pending)
	got := machineNames(pending)
	want := []string{"c", "a", "b"}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %v, want %v (ties must preserve arrival order)", got, want)
	}
}

func TestSortByPriorityDescEmptyAndSingle(t *testing.T) {
	var empty []model.Machine
	SortByPriorityDesc(empty) // must not panic

	one := []model.Machine{namedMachine("solo", 3)}
	SortByPriorityDesc(one)
	if len(one) != 1 || one[0].Metadata.Name != "solo" {
		t.Fatalf("got %v", one)
	}
}
