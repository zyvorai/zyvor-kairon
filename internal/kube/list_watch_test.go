// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestListMachinesWithSelector(t *testing.T) {
	var gotSelector string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/kairon.zyvor.dev/v1/machines" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		gotSelector = r.URL.Query().Get("labelSelector")
		_ = json.NewEncoder(w).Encode(model.MachineList{Items: []model.Machine{{
			Metadata: model.ObjectMeta{Name: "vm1", Namespace: "default", Labels: map[string]string{model.AssignedNodeLabel: "worker-1"}},
			Spec:     model.MachineSpec{NodeName: "worker-1"},
		}}})
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()
	items, err := c.ListMachinesWithSelector(context.Background(), model.AssignedNodeLabelSelector("worker-1"))
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if gotSelector != model.AssignedNodeLabelSelector("worker-1") {
		t.Fatalf("selector=%q", gotSelector)
	}
}

func TestWatchMachinesStreamsEvents(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("watch") != "true" {
			http.Error(w, "expected watch", http.StatusBadRequest)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"ADDED","object":{"metadata":{"name":"vm1"}}}` + "\n"))
		flusher.Flush()
	}))
	defer s.Close()
	c, _ := New(s.URL, "", "", false)
	c.HTTP = s.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := make(chan WatchEvent, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- c.WatchMachines(ctx, model.AssignedNodeLabelSelector("worker-1"), "", ch) }()
	select {
	case ev := <-ch:
		if ev.Type != "ADDED" {
			t.Fatalf("type=%s", ev.Type)
		}
		cancel()
	case err := <-errCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for watch event")
	}
}
