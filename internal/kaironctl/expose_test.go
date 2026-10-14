// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestExposeService(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web-1", UID: "m-uid"}}
	svc, err := exposeService(m, "prod", exposeOptions{Type: "NodePort", Protocol: "tcp", Port: 22})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Metadata.Name != "web-1" || svc.Metadata.Labels[kube.LabelExposeMachine] != "web-1" || svc.Spec.Selector != nil {
		t.Fatalf("meta %+v", svc)
	}
	own := svc.Metadata.OwnerReferences[0]
	if own.Kind != "Machine" || own.UID != "m-uid" || own.APIVersion != model.APIVersion {
		t.Fatalf("owner %+v", own)
	}
	p := svc.Spec.Ports[0]
	if p.Name != "tcp-22" || p.Protocol != "TCP" || p.TargetPort.Int != 22 {
		t.Fatalf("port %+v", p)
	}

	bad := []exposeOptions{
		{Type: "ExternalName", Protocol: "TCP", Port: 80},
		{Type: "ClusterIP", Protocol: "ICMP", Port: 80},
		{Type: "ClusterIP", Protocol: "TCP"},
		{Type: "ClusterIP", Protocol: "TCP", Port: 80, NodePort: 30080},
		{Type: "ClusterIP", Protocol: "TCP", Port: 80, Name: "Web_1"},
	}
	for _, o := range bad {
		if _, err := exposeService(m, "prod", o); err == nil {
			t.Fatalf("%+v accepted", o)
		}
	}
}

func TestWriteExposed(t *testing.T) {
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web-1"}, Status: model.MachineStatus{Phase: "Running", GuestIP: "10.0.0.9"}}
	svc, _ := exposeService(m, "prod", exposeOptions{Type: "ClusterIP", Protocol: "TCP", Port: 80, TargetPort: 8080})
	svc.Spec.ClusterIP = "10.96.0.12"
	var b strings.Builder
	writeExposed(&b, svc, kube.DesiredExposeSlice(svc, &m))
	if !strings.Contains(b.String(), "web-1.prod.svc:80/TCP -> 10.0.0.9:8080") || !strings.Contains(b.String(), "10.96.0.12") {
		t.Fatalf("%s", b.String())
	}
	b.Reset()
	m.Status.GuestIP = ""
	writeExposed(&b, svc, kube.DesiredExposeSlice(svc, &m))
	if !strings.Contains(b.String(), "no guest IP yet") {
		t.Fatalf("%s", b.String())
	}
}
