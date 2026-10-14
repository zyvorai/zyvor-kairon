// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestReconcileExposedServicesFollowsGuestIP(t *testing.T) {
	svc := kube.Service{
		Metadata: kube.ObjectMeta{Name: "web", Namespace: "prod", UID: "u", Labels: map[string]string{kube.LabelExposeMachine: "web-1"}},
		Spec:     kube.ServiceSpec{Ports: []kube.ServicePort{{Name: "tcp-80", Protocol: "TCP", Port: 80}}},
	}
	var created []kube.EndpointSlice
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/services":
			if r.URL.Query().Get("labelSelector") != kube.LabelExposeMachine {
				t.Errorf("selector %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(kube.ServiceList{Items: []kube.Service{svc}})
		case r.Method == http.MethodGet && r.URL.Path == "/apis/discovery.k8s.io/v1/namespaces/prod/endpointslices/web-kairon":
			http.Error(w, `{"reason":"NotFound"}`, http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/apis/discovery.k8s.io/v1/namespaces/prod/endpointslices":
			var s kube.EndpointSlice
			_ = json.NewDecoder(r.Body).Decode(&s)
			created = append(created, s)
			_ = json.NewEncoder(w).Encode(s)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := &Controller{Kube: &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "web-1", Namespace: "prod"}, Status: model.MachineStatus{Phase: "Running", GuestIP: "10.0.0.7"}}
	c.reconcileExposedServices(context.Background(), []model.Machine{m})
	if len(created) != 1 || created[0].Endpoints[0].Addresses[0] != "10.0.0.7" || created[0].Metadata.Labels[kube.LabelServiceName] != "web" {
		t.Fatalf("created %+v", created)
	}
}

func TestReconcileExposedServicesWarnsOnceWithoutRBAC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	var logs countingHandler
	c := &Controller{Kube: &kube.Client{BaseURL: srv.URL, HTTP: srv.Client()}, Log: slog.New(&logs)}
	c.reconcileExposedServices(context.Background(), nil)
	c.reconcileExposedServices(context.Background(), nil)
	if logs.n != 1 {
		t.Fatalf("logged %d times", logs.n)
	}
}

type countingHandler struct{ n int }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h *countingHandler) Handle(context.Context, slog.Record) error { h.n++; return nil }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *countingHandler) WithGroup(string) slog.Handler             { return h }
