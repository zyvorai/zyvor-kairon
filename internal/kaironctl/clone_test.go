// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestCloneMachine(t *testing.T) {
	src := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-1", Namespace: "dev",
			Labels: map[string]string{
				"app": "web", "app.kubernetes.io/version": "3", "team": "a",
				model.LabelMachinePool: "p", model.AssignedNodeLabel: "old",
			},
			Annotations: map[string]string{"note": "x", model.AnnotationLastActivity: "t", "kubectl.kubernetes.io/last-applied-configuration": "{}"},
		},
		Spec: model.MachineSpec{
			NodeName: "n1", PowerState: "Halted",
			Network:   model.NetworkSpec{MAC: "52:54:00:00:00:01"},
			CloudInit: model.CloudInitSpec{Hostname: "web-1"},
		},
		Status: model.MachineStatus{Phase: "Halted"},
	}

	m, err := cloneMachine(src, "copy", []string{"app*", "!app.kubernetes.io/version"}, []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Metadata.Annotations[model.AnnotationCloneFrom] != "web-1" || m.Metadata.Labels[model.LabelClonedFrom] != "web-1" {
		t.Fatalf("halted source must be disk-copied: %+v", m.Metadata)
	}
	wantLabels := map[string]string{"app": "web", model.AssignedNodeLabel: "n1", model.LabelClonedFrom: "web-1"}
	if len(m.Metadata.Labels) != len(wantLabels) {
		t.Fatalf("labels %v", m.Metadata.Labels)
	}
	for k, v := range wantLabels {
		if m.Metadata.Labels[k] != v {
			t.Fatalf("labels %v", m.Metadata.Labels)
		}
	}
	if len(m.Metadata.Annotations) != 2 || m.Metadata.Annotations["note"] != "x" {
		t.Fatalf("annotations %v", m.Metadata.Annotations)
	}
	if m.Spec.PowerState != "Running" || m.Spec.Network.MAC != "" || m.Spec.CloudInit.Hostname != "copy" || m.Spec.NodeName != "n1" {
		t.Fatalf("spec %+v", m.Spec)
	}

	src.Status.Phase = "Running"
	m, err = cloneMachine(src, "copy", nil, nil)
	if err != nil || m.Metadata.Annotations[model.AnnotationForkFrom] != "web-1" || len(m.Metadata.Annotations) != 1 {
		t.Fatalf("running source must fork: %v %+v", err, m.Metadata)
	}
	if m.Metadata.Labels["team"] != "a" || m.Metadata.Labels[model.LabelMachinePool] != "" {
		t.Fatalf("default label filter: %v", m.Metadata.Labels)
	}

	src.Status.Phase = "Stopped"
	if _, err := cloneMachine(src, "copy", nil, nil); err == nil || !strings.Contains(err.Error(), "Stopped") {
		t.Fatalf("stopped: %v", err)
	}
	src.Status.Phase = "Halted"
	src.Spec.Volumes = []model.MachineVolume{{Name: "root", ClaimName: "c"}}
	if _, err := cloneMachine(src, "copy", nil, nil); err == nil {
		t.Fatal("volumes accepted")
	}
}
