// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func exposedSvc() Service {
	return Service{
		Metadata: ObjectMeta{Name: "web", Namespace: "prod", UID: "svc-uid", Labels: map[string]string{LabelExposeMachine: "web-1"}},
		Spec: ServiceSpec{Ports: []ServicePort{
			{Name: "tcp-80", Protocol: "TCP", Port: 80, TargetPort: &IntOrString{Int: 8080}},
			{Name: "named", Protocol: "TCP", Port: 443, TargetPort: &IntOrString{Str: "https"}},
		}},
	}
}

func TestDesiredExposeSlice(t *testing.T) {
	m := &model.Machine{Status: model.MachineStatus{Phase: "Running", GuestIP: "10.1.2.3", NodeName: "n1"}}
	s := DesiredExposeSlice(exposedSvc(), m)
	if s.Metadata.Name != "web-kairon" || s.Metadata.Labels[LabelServiceName] != "web" || s.Metadata.OwnerReferences[0].UID != "svc-uid" {
		t.Fatalf("meta %+v", s.Metadata)
	}
	if s.AddressType != "IPv4" || len(s.Endpoints) != 1 || s.Endpoints[0].Addresses[0] != "10.1.2.3" || !*s.Endpoints[0].Conditions.Ready {
		t.Fatalf("endpoints %+v", s)
	}
	if *s.Ports[0].Port != 8080 || *s.Ports[1].Port != 443 {
		t.Fatalf("ports %v %v", *s.Ports[0].Port, *s.Ports[1].Port)
	}

	m.Status.Phase, m.Status.GuestIP = "Stopped", "fd00::5"
	s = DesiredExposeSlice(exposedSvc(), m)
	if s.AddressType != "IPv6" || *s.Endpoints[0].Conditions.Ready {
		t.Fatalf("stopped v6 %+v", s)
	}
	if s = DesiredExposeSlice(exposedSvc(), nil); len(s.Endpoints) != 0 {
		t.Fatalf("missing machine %+v", s)
	}
}

func TestIntOrStringJSON(t *testing.T) {
	var p ServicePort
	if err := json.Unmarshal([]byte(`{"port":80,"targetPort":"http"}`), &p); err != nil || p.TargetPort.Str != "http" {
		t.Fatalf("%+v %v", p, err)
	}
	b, _ := json.Marshal(ServicePort{Port: 80, TargetPort: &IntOrString{Int: 8080}})
	if string(b) != `{"port":80,"targetPort":8080}` {
		t.Fatalf("%s", b)
	}
}

func TestSyncExposeSlice(t *testing.T) {
	stored := DesiredExposeSlice(exposedSvc(), &model.Machine{Status: model.MachineStatus{Phase: "Running", GuestIP: "10.0.0.1"}})
	stored.Metadata.ResourceVersion = "7"
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(stored)
		case http.MethodPut:
			puts++
			var s EndpointSlice
			_ = json.NewDecoder(r.Body).Decode(&s)
			if r.URL.Path != "/apis/discovery.k8s.io/v1/namespaces/prod/endpointslices/web-kairon" || s.Metadata.ResourceVersion != "7" {
				t.Errorf("put %s rv=%q", r.URL.Path, s.Metadata.ResourceVersion)
			}
			stored = s
			_ = json.NewEncoder(w).Encode(s)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	m := &model.Machine{Status: model.MachineStatus{Phase: "Running", GuestIP: "10.0.0.1"}}
	if _, err := c.SyncExposeSlice(context.Background(), exposedSvc(), m); err != nil || puts != 0 {
		t.Fatalf("unchanged slice rewritten: %v puts=%d", err, puts)
	}
	m.Status.GuestIP = "10.0.0.2"
	if _, err := c.SyncExposeSlice(context.Background(), exposedSvc(), m); err != nil || puts != 1 || stored.Endpoints[0].Addresses[0] != "10.0.0.2" {
		t.Fatalf("ip change not written: %v puts=%d", err, puts)
	}
}
