// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestInsertEjectCdrom(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "win", ResourceVersion: "7"},
		Spec:     model.MachineSpec{PowerState: "Stopped", Cdroms: []model.MachineCdrom{{Name: "install", ImageRef: "ws2022"}}},
	}
	got, err := insertCdrom(m, model.MachineCdrom{Name: "virtio", ImageRef: "virtio-win"})
	if err != nil || len(got) != 2 || got[1].Name != "virtio" || len(m.Spec.Cdroms) != 1 {
		t.Fatalf("insert: %v %+v", err, got)
	}
	if _, err := insertCdrom(m, model.MachineCdrom{Name: "install", ImageRef: "x"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	running := m
	running.Spec.PowerState, running.Status.RuntimeID = "Running", "vm-1"
	if _, err := insertCdrom(running, model.MachineCdrom{Name: "virtio", ImageRef: "virtio-win"}); err == nil || !strings.Contains(err.Error(), "stop the Machine") {
		t.Fatalf("running: %v", err)
	}
	out, err := ejectCdrom(running, "install")
	if err != nil || len(out) != 0 {
		t.Fatalf("eject: %v %+v", err, out)
	}
	p := cdromsPatch(m, out)
	if p["metadata"].(map[string]any)["resourceVersion"] != "7" || p["spec"].(map[string]any)["cdroms"] == nil {
		t.Fatalf("patch %v", p)
	}
	if _, err := ejectCdrom(m, "nope"); err == nil {
		t.Fatal("unknown cdrom ejected")
	}
}

func TestMachineManifest(t *testing.T) {
	m := model.Machine{
		Metadata: model.ObjectMeta{Name: "web-1", Namespace: "dev", UID: "u", ResourceVersion: "9",
			Labels:      map[string]string{"app": "web", model.AssignedNodeLabel: "n1"},
			Annotations: map[string]string{model.AnnotationLastActivity: "t", "owner": "me"},
		},
		Spec:   model.MachineSpec{NodeName: "n1", Network: model.NetworkSpec{TapName: "tap0", MAC: "52:54:00:00:00:01"}},
		Status: model.MachineStatus{Phase: "Running"},
	}
	svc := kube.Service{
		Metadata: kube.ObjectMeta{Name: "web-1-ssh", Namespace: "dev", UID: "x", Labels: map[string]string{kube.LabelExposeMachine: "web-1"}},
		Spec:     kube.ServiceSpec{Type: "NodePort", ClusterIP: "10.0.0.5", Ports: []kube.ServicePort{{Port: 22, NodePort: 30022}}},
	}
	var buf bytes.Buffer
	if err := writeMachineManifest(&buf, manifestMachine(m, false), []kube.Service{manifestService(svc)}, "yaml"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, bad := range []string{"uid", "resourceVersion", "status", "nodeName", "tap0", "10.0.0.5", "30022", "namespace", model.AnnotationLastActivity, model.AssignedNodeLabel} {
		if strings.Contains(out, bad) {
			t.Fatalf("manifest keeps %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"kind: Machine", "---", "kind: Service", "app: web", "owner: me", "52:54:00:00:00:01", "port: 22"} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifest misses %q:\n%s", want, out)
		}
	}
	buf.Reset()
	if err := writeMachineManifest(&buf, manifestMachine(m, true), nil, "json"); err != nil || !strings.Contains(buf.String(), `"kind": "List"`) || !strings.Contains(buf.String(), `"nodeName": "n1"`) {
		t.Fatalf("json: %v\n%s", err, buf.String())
	}
}
