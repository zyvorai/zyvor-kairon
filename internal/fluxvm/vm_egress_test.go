// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestWireEgressAndEquivalence(t *testing.T) {
	if WireEgress(nil) != nil {
		t.Fatal("nil spec must stay nil so the create request omits the field")
	}
	w := WireEgress(&model.EgressSpec{Rules: []model.EgressRule{
		{CIDR: "10.0.0.0/8", Proto: "tcp", Port: 5151, Note: "veyron"},
		{CIDR: "192.168.1.5"},
	}})
	if w.Mode != "allowlist" || w.Rules[1].Proto != "any" {
		t.Fatalf("defaults not applied: %+v", w)
	}
	b, _ := json.Marshal(w)
	want := `{"mode":"allowlist","rules":[{"cidr":"10.0.0.0/8","proto":"tcp","port":5151,"note":"veyron"},{"cidr":"192.168.1.5","proto":"any"}]}`
	if string(b) != want {
		t.Fatalf("wire JSON\n got %s\nwant %s", b, want)
	}
	// FluxVM reports the same policy in a different order, with a bare address spelled /32 and no notes.
	got := EgressPolicy{Mode: "allowlist", Rules: []EgressRule{
		{CIDR: "192.168.1.5/32", Proto: "any"}, {CIDR: "10.0.0.0/8", Proto: "tcp", Port: 5151},
	}}
	if !w.Equivalent(got) {
		t.Fatal("equivalent policies compared as different; the reconciler would rewrite them every tick")
	}
	got.Rules[1].Port = 5152
	if w.Equivalent(got) {
		t.Fatal("different port compared as equal")
	}
}

func TestEgressClientCalls(t *testing.T) {
	var calls []string
	var putBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut {
			putBody = string(b)
		}
		_, _ = w.Write([]byte(`{"policy":{"mode":"audit","rules":[]},"source":"node_default"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	p, src, err := c.GetVMEgress(context.Background(), "vm-1")
	if err != nil || p.Mode != "audit" || src != "node_default" {
		t.Fatalf("get: %+v %q %v", p, src, err)
	}
	if err := c.SetVMEgress(context.Background(), "vm-1", EgressPolicy{Mode: "open", Rules: []EgressRule{}}); err != nil {
		t.Fatal(err)
	}
	if putBody != `{"mode":"open","rules":[]}` {
		t.Fatalf("put body %s", putBody)
	}
	if err := c.ClearVMEgress(context.Background(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/vms/vm-1/network/egress", "PUT /v1/vms/vm-1/network/egress", "DELETE /v1/vms/vm-1/network/egress"}
	for i, w := range want {
		if calls[i] != w {
			t.Fatalf("call %d = %q, want %q", i, calls[i], w)
		}
	}
}
