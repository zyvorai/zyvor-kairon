// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildObjectGraph(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-1", Namespace: "dev", Labels: map[string]string{
			"app":                   "web",
			model.LabelMachinePool:  "py",
			model.LabelMachineClaim: "c1",
		}},
		Spec: model.MachineSpec{
			InstanceTypeName: "small",
			InstanceTypeKind: "MachineClusterInstanceType",
			Volumes:          []model.MachineVolume{{Name: "data", ClaimName: "web-1-data"}},
		},
		Status: model.MachineStatus{Phase: "Running", NodeName: "n1"},
	}
	rel := machineRelated{
		Claims: []model.MachineClaim{{Metadata: model.ObjectMeta{Name: "c1"}, Status: model.MachineClaimStatus{Phase: "Bound"}}},
		Snapshots: []model.MachineSnapshot{
			{Metadata: model.ObjectMeta{Name: "s1"}, Spec: model.MachineSnapshotSpec{MachineName: "web-1"}},
			{Metadata: model.ObjectMeta{Name: "other"}, Spec: model.MachineSnapshotSpec{MachineName: "web-2"}},
		},
		NetPolicies: []model.MachineNetworkPolicy{
			{Metadata: model.ObjectMeta{Name: "by-label"}, Spec: model.MachineNetworkPolicySpec{Selector: map[string]string{"app": "web"}}},
			{Metadata: model.ObjectMeta{Name: "pinned-elsewhere"}, Spec: model.MachineNetworkPolicySpec{MachineName: "web-2", Selector: map[string]string{"app": "web"}}},
		},
		Budgets: []model.MachineDisruptionBudget{{Metadata: model.ObjectMeta{Name: "pdb"}, Spec: model.MachineDisruptionBudgetSpec{Selector: map[string]string{"app": "db"}}}},
		Services: []kube.Service{
			{Metadata: kube.ObjectMeta{Name: "web-1-ssh", Namespace: "dev", Labels: map[string]string{kube.LabelExposeMachine: "web-1"}}},
			{Metadata: kube.ObjectMeta{Name: "web-1-ssh", Namespace: "prod", Labels: map[string]string{kube.LabelExposeMachine: "web-1"}}},
		},
	}
	g := buildObjectGraph(m, rel)
	var got []string
	for _, c := range g.Children {
		got = append(got, c.Kind+"/"+c.Namespace+"/"+c.Name+"/"+c.Status)
	}
	want := []string{
		"MachineClaim/dev/c1/Bound",
		"MachineClusterInstanceType//small/",
		"MachineNetworkPolicy/dev/by-label/",
		"MachinePool/dev/py/",
		"MachineSnapshot/dev/s1/",
		"Node//n1/",
		"PersistentVolumeClaim/dev/web-1-data/",
		"Service/dev/web-1-ssh/",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("children:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, format := range []string{"tree", "json", "yaml", "dot"} {
		var buf bytes.Buffer
		if err := writeObjectGraph(&buf, g, format); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(buf.String(), "web-1-data") {
			t.Fatalf("%s output misses a child:\n%s", format, buf.String())
		}
	}
	var buf bytes.Buffer
	_ = writeObjectGraph(&buf, g, "json")
	var round graphNode
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil || len(round.Children) != len(want) {
		t.Fatalf("json round trip: %v %+v", err, round)
	}
	if err := writeObjectGraph(&buf, g, "svg"); err == nil {
		t.Fatal("unknown format accepted")
	}
}
