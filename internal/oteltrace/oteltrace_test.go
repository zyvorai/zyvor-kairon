// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package oteltrace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStartEndNoOpWithoutEndpoint(t *testing.T) {
	tr := &Tracer{}
	sp := tr.Start(context.Background(), "reconcile.machine", map[string]string{"kind": "machine"})
	sp.End(nil) // must not panic or dial
}

func TestEndPostsOTLPJSON(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := &Tracer{Endpoint: srv.URL + "/v1/traces", ServiceName: "kairon-controller", HTTP: srv.Client()}
	sp := tr.Start(context.Background(), "reconcile.machine", map[string]string{"kind": "machine", "name": "db"})
	sp.End(nil)
	if len(gotBody) == 0 {
		t.Fatal("expected an OTLP/HTTP JSON body")
	}
	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("json: %v body=%s", err, gotBody)
	}
	raw, _ := json.Marshal(payload)
	if !strings.Contains(string(raw), "reconcile.machine") || !strings.Contains(string(raw), "kairon-controller") {
		t.Fatalf("payload missing span/service: %s", raw)
	}
}
