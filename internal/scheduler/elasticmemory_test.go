// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func elasticMachine(name, mem, min string) model.Machine {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: name},
		Spec:     model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: mem}, PowerState: "Running"},
	}
	if min != "" {
		m.Spec.Resources.ElasticMemory = &model.ElasticMemorySpec{Min: min}
	}
	return m
}

func overcommitNode(ratio string) model.Node {
	n := node("a", true, true)
	n.Status.Allocatable = map[string]string{"cpu": "16", "memory": "8Gi"}
	if ratio != "" {
		n.Metadata.Annotations = map[string]string{model.AnnotationMemoryOvercommitRatio: ratio}
	}
	return n
}

func TestMemoryOvercommitRatio(t *testing.T) {
	cases := map[string]float64{"": 1, "1.5": 1.5, "0.5": 1, "junk": 1, "9": MaxMemoryOvercommitRatio}
	for raw, want := range cases {
		if got := MemoryOvercommitRatio(overcommitNode(raw)); got != want {
			t.Errorf("ratio %q: got %v want %v", raw, got, want)
		}
	}
}

func TestOvercommitPlacesElasticMachinesBeyondAllocatable(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := overcommitNode("2")
	load := map[string]NodeLoad{}
	// Three 4Gi Machines with a 2Gi minimum: 12Gi full (limit 16Gi) and
	// 6Gi guaranteed (allocatable 8Gi).
	for _, name := range []string{"a1", "a2", "a3"} {
		m := elasticMachine(name, "4Gi", "2Gi")
		got, err := s.Choose(m, []model.Node{n}, nil, load, "")
		if err != nil || got != "a" {
			t.Fatalf("%s: %q %v", name, got, err)
		}
		Reserve(load, "a", m)
	}
	if l := load["a"]; l.MemoryMiB != 12*1024 || l.GuaranteedMemoryMiB != 6*1024 {
		t.Fatalf("load %+v", l)
	}
	// A fourth would push guaranteed memory to 8Gi (fits) and full memory
	// to 16Gi (fits); a fifth exceeds the guaranteed share.
	m4 := elasticMachine("a4", "4Gi", "2Gi")
	if _, err := s.Choose(m4, []model.Node{n}, nil, load, ""); err != nil {
		t.Fatalf("a4: %v", err)
	}
	Reserve(load, "a", m4)
	_, err := s.Choose(elasticMachine("a5", "1Gi", "512Mi"), []model.Node{n}, nil, load, "")
	if err == nil || !strings.Contains(err.Error(), "insufficient guaranteed memory") {
		t.Fatalf("a5: %v", err)
	}
}

func TestOvercommitLimitsFullMemory(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := overcommitNode("1.5")
	load := map[string]NodeLoad{"a": {Count: 1, MemoryMiB: 10 * 1024, GuaranteedMemoryMiB: 2 * 1024}}
	_, err := s.Choose(elasticMachine("big", "4Gi", "1Gi"), []model.Node{n}, nil, load, "")
	if err == nil || !strings.Contains(err.Error(), "overcommit limit 12288Mi") {
		t.Fatalf("err=%v", err)
	}
}

func TestNonElasticMachineCountsFullyAgainstGuaranteed(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := overcommitNode("4")
	load := map[string]NodeLoad{"a": {Count: 1, MemoryMiB: 8 * 1024, GuaranteedMemoryMiB: 4 * 1024}}
	_, err := s.Choose(elasticMachine("plain", "6Gi", ""), []model.Node{n}, nil, load, "")
	if err == nil || !strings.Contains(err.Error(), "insufficient guaranteed memory") {
		t.Fatalf("err=%v", err)
	}
}

func TestWithoutRatioElasticMachinesCountFully(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	n := overcommitNode("")
	load := map[string]NodeLoad{"a": {Count: 1, MemoryMiB: 6 * 1024, GuaranteedMemoryMiB: 1024}}
	_, err := s.Choose(elasticMachine("e", "4Gi", "1Gi"), []model.Node{n}, nil, load, "")
	if err == nil || !strings.Contains(err.Error(), "insufficient memory") {
		t.Fatalf("err=%v", err)
	}
}

func TestMemoryPressureNodeIsSkipped(t *testing.T) {
	s := Scheduler{RequireCapableLabel: true}
	a := overcommitNode("")
	a.Status.Conditions = append(a.Status.Conditions, model.NodeCondition{Type: model.NodeConditionKaironMemoryPressure, Status: "True"})
	b := node("b", true, true)
	got, err := s.Choose(elasticMachine("vm", "1Gi", ""), []model.Node{a, b}, nil, map[string]NodeLoad{}, "")
	if err != nil || got != "b" {
		t.Fatalf("got %q err=%v", got, err)
	}
	_, err = s.Choose(elasticMachine("vm", "1Gi", ""), []model.Node{a}, nil, map[string]NodeLoad{}, "")
	if err == nil || !strings.Contains(err.Error(), model.NodeConditionKaironMemoryPressure) {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildNodeLoadCountsGuaranteedMemory(t *testing.T) {
	e := elasticMachine("e", "4Gi", "1Gi")
	e.Spec.NodeName = "a"
	p := elasticMachine("p", "2Gi", "")
	p.Spec.NodeName = "a"
	l := BuildNodeLoad([]model.Machine{e, p})["a"]
	if l.MemoryMiB != 6*1024 || l.GuaranteedMemoryMiB != 3*1024 || l.Count != 2 {
		t.Fatalf("load %+v", l)
	}
}
