// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func TestEdgeDropsAndBeaconBecomeDedupedWarningEvents(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms/vm-1/network/drops":
			_, _ = io.WriteString(w, `{"items":[{"reason":"spoof_ip","srcIP":"10.9.9.9","dstIP":"10.0.0.1","packets":3}]}`)
		case "/v1/vms/vm-1/network/flows":
			_, _ = io.WriteString(w, `{"items":[{"dstIP":"203.0.113.7","bytes":64,"intervalSec":30},{"dstIP":"203.0.113.7","bytes":64,"intervalSec":30},{"dstIP":"203.0.113.7","bytes":64,"intervalSec":31}]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer fs.Close()
	var mu sync.Mutex
	reasons := map[string]int{}
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/demo/events" {
			var ev model.Event
			_ = json.NewDecoder(r.Body).Decode(&ev)
			mu.Lock()
			reasons[ev.Reason]++
			mu.Unlock()
			if ev.Type != "Warning" || ev.InvolvedObject.Name != "web" {
				t.Errorf("event %+v", ev)
			}
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := model.Machine{Metadata: model.ObjectMeta{Namespace: "demo", Name: "web", UID: "u1"}}

	a.observeEdgeDrops(context.Background(), m, "vm-1")
	a.observeEdgeDrops(context.Background(), m, "vm-1")

	mu.Lock()
	defer mu.Unlock()
	if reasons["EdgeDrop"] != 1 || reasons["EdgeAnomaly"] != 1 {
		t.Fatalf("events = %v, want one EdgeDrop and one EdgeAnomaly", reasons)
	}
}

func TestEdgeBaselineDeviationFromSnapshot(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/vms/vm-1/network/flows" {
			_, _ = io.WriteString(w, `{"items":[{"dstIP":"198.51.100.1","sni":"a.example","bytes":90000000}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	defer fs.Close()
	var mu sync.Mutex
	var messages []string
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev model.Event
		_ = json.NewDecoder(r.Body).Decode(&ev)
		mu.Lock()
		messages = append(messages, ev.Message)
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ks.Close()

	path := filepath.Join(t.TempDir(), "baseline.json")
	warm := agentplane.NewBaseline()
	key := agentplane.BaselineKey("acme", "demo", "web")
	for i := 0; i < warm.Warmup+5; i++ {
		warm.Observe(key, agentplane.Sample{Bytes: 100000, Flows: 1, Dsts: 1, SNIs: 1})
	}
	if err := warm.Save(path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	kc, _ := kube.New(ks.URL, "", "", false)
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Kube: kc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), EdgeBaselinePath: path}
	m := model.Machine{Metadata: model.ObjectMeta{Namespace: "demo", Name: "web", Labels: map[string]string{"kairon.zyvor.dev/tenant": "acme"}}}
	a.baseline()
	a.edgeBaseline.saved = time.Time{}
	a.observeEdgeDrops(context.Background(), m, "vm-1")

	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "baseline_deviation: bytes ") {
		t.Fatalf("messages = %q", messages)
	}
	if after, _ := os.Stat(path); !after.ModTime().After(before.ModTime()) && after.Size() == before.Size() {
		t.Fatal("snapshot not rewritten")
	}
}

func TestEdgeEventDedupExpires(t *testing.T) {
	var d edgeEventDedup
	now := time.Now()
	if !d.allow("k", now) || d.allow("k", now.Add(time.Minute)) {
		t.Fatal("second event inside window should be suppressed")
	}
	if !d.allow("k", now.Add(edgeEventWindow)) {
		t.Fatal("event after window should pass")
	}
}

func TestParseEdgeFlowsAcceptsSnakeCase(t *testing.T) {
	flows := parseEdgeFlows(json.RawMessage(`{"items":[{"dst_ip":"1.2.3.4","qname":"x.example","bytes":10}]}`))
	if len(flows) != 1 || flows[0].DstIP != "1.2.3.4" || flows[0].DNS != "x.example" {
		t.Fatalf("%+v", flows)
	}
	flows = parseEdgeFlows(json.RawMessage(`{"items":[{"identity":3451097839,"family":6,"source":"fe80::1","destination":"ff02::2","protocol":58,"verdict":"allow","packets":1,"bytes":70}]}`))
	if len(flows) != 1 || flows[0].DstIP != "ff02::2" || flows[0].Bytes != 70 {
		t.Fatalf("FluxVM flow shape: %+v", flows)
	}
}
