// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
)

func TestClientReusesConnectionAfterRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer s.Close()
	c, err := New(s.URL, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	if _, err := c.ListMachines(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reused bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	if _, err := c.ListMachines(ctx); err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("second reconciliation request opened a new connection")
	}
}

func BenchmarkClientListMachines(b *testing.B) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer s.Close()
	c, err := New(s.URL, "", "", false)
	if err != nil {
		b.Fatal(err)
	}
	defer c.HTTP.CloseIdleConnections()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := c.ListMachines(context.Background()); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
