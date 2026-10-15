// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/metrics"
	"github.com/zyvorai/kairon/internal/migration"
	"github.com/zyvorai/kairon/internal/model"
)

func TestApplyEdgeProjectsStableIdentity(t *testing.T) {
	var posted bool
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/network/edge" {
			posted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer fs.Close()
	mbps := uint32(100)
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec: model.MachineSpec{Network: model.NetworkSpec{
			DataplaneMode: "ebpf",
			AntiSpoof:     true,
			MAC:           "52:54:00:aa:bb:cc",
			QoS:           &model.NetworkQoS{EgressMbps: &mbps},
		}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", "")
	if err != nil {
		t.Fatal(err)
	}
	if !posted || st == nil || st.Identity != ebpfedge.StableIdentity("demo", "web") || !st.AntiSpoof {
		t.Fatalf("status=%+v posted=%v", st, posted)
	}
	if !st.Applied || st.ApplyError != "" || len(st.SpecHash) != 16 {
		t.Fatalf("accepted apply must report applied with a hash: %+v", st)
	}
}

func TestApplyEdgeFailureIsVisibleWhenNotRequired(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no edge", http.StatusNotImplemented)
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{DataplaneMode: "ebpf"}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := a.applyEdge(context.Background(), m, "vm-1", "", "")
	if err != nil || st == nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if st.Applied || st.ApplyError == "" || st.SpecHash == "" {
		t.Fatalf("a failed apply must not look applied: %+v", st)
	}
}

func TestApplyEdgeFailClosedWhenRequired(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no edge", http.StatusNotImplemented)
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{DataplaneMode: "ebpf", DataplaneRequired: true}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := a.applyEdge(context.Background(), m, "vm-1", "", ""); err == nil {
		t.Fatal("expected fail-closed edge apply")
	}
}

func TestExportConntrackStampsIdentity(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[{"proto":"tcp","srcIP":"10.0.0.8","dstIP":"10.1.0.4","srcPort":1,"dstPort":443,"state":"established"}]}`))
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{AntiSpoof: true}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	raw := a.exportConntrackSnapshot(context.Background(), m, "vm-1")
	snap, err := ebpfedge.UnmarshalSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Identity != ebpfedge.StableIdentity("demo", "web") || len(snap.Entries) != 1 || snap.ExportedAt.IsZero() {
		t.Fatalf("snap=%+v", snap)
	}
	if _, err := ebpfedge.RestoreConntrack(snap.Identity, snap, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeNotRequestedSkipsFlux(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("flux should not be called")
	}))
	defer fs.Close()
	m := model.Machine{Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"}}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := a.applyEdge(context.Background(), m, "vm-1", "", "")
	if err != nil || st != nil {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if raw := a.exportConntrackSnapshot(context.Background(), m, "vm-1"); raw != nil {
		t.Fatalf("unexpected snapshot %s", raw)
	}
}

func TestMergeSelectingPolicyCopiesSNIAndName(t *testing.T) {
	spec := buildEdgeSpec(model.Machine{Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"}}, "10.0.0.8")
	mbps := uint32(50)
	merged := mergeSelectingPolicy(spec, model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Name: "egress-443"},
		Spec: model.MachineNetworkPolicySpec{Policy: model.VmNetworkPolicy{
			DefaultAllow:  false,
			AllowSNI:      []string{"*.vendor.com"},
			AllowFqdns:    []string{"api.vendor.com"},
			MaxEgressMbps: &mbps,
		}},
	})
	if merged.PolicyName != "egress-443" || merged.DefaultAllow || merged.AllowSNI[0] != "*.vendor.com" || merged.AllowDNS[0] != "api.vendor.com" || merged.QoS.EgressMbps != 50 {
		t.Fatalf("merged %+v", merged)
	}
}

func TestApplyEdgeProjectsConntrackRestore(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	store := migration.NewRestoreStore()
	store.Put("demo", "web", ebpfedge.RestoreResult{Restored: 4, BlackholeWindowMs: 25, Identity: 1})
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{DataplaneMode: "ebpf"}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Restores: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.ConntrackRestored != 4 || st.BlackholeWindowMs != 25 || st.GuestIPSource != ebpfedge.IPSourceAgent {
		t.Fatalf("status %+v", st)
	}
}

func TestApplyEdgeLearnsIPWhenUnset(t *testing.T) {
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/vm-1/network/learned-ip" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ip":"10.0.0.15","source":"dhcp"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{DataplaneMode: "ebpf", LearnIP: true}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	st, err := a.applyEdge(context.Background(), m, "vm-1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if st.GuestIPSource != "dhcp" {
		t.Fatalf("source %q", st.GuestIPSource)
	}
}

func TestApplyEdgeTurnsOnForPolicyOnlyFields(t *testing.T) {
	var body []byte
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/vms/vm-1/network/edge" {
			body, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web", Labels: map[string]string{"app": "web"}},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{Mode: "tap", NetNS: true, DataplaneMode: "cilium"}},
	}
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	plain := model.MachineNetworkPolicy{Metadata: model.ObjectMeta{Namespace: "demo", Name: "cidrs"}}
	plain.Spec.Selector = map[string]string{"app": "web"}
	plain.Spec.Policy.AllowCidrs = []string{"10.0.0.0/8"}
	a.edge.setPolicies([]model.MachineNetworkPolicy{plain})
	if st, err := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", "52:54:00:00:00:01"); err != nil || st != nil || body != nil {
		t.Fatalf("policy without edge fields: st=%+v err=%v body=%s", st, err, body)
	}

	sni := plain
	sni.Metadata.Name = "sni"
	sni.Spec.Policy.AllowSNI = []string{"*.example.com"}
	a.edge.setPolicies([]model.MachineNetworkPolicy{sni})
	st, err := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", "52:54:00:00:00:01")
	if err != nil || st == nil || st.PolicyName != "sni" {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	var posted ebpfedge.EdgeSpec
	if err := json.Unmarshal(body, &posted); err != nil {
		t.Fatal(err)
	}
	if len(posted.AllowSNI) != 1 || posted.AssignedMAC != "52:54:00:00:00:01" {
		t.Fatalf("posted %+v", posted)
	}

	m.Spec.Network.Mode = "user"
	body = nil
	if st, _ := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", ""); st != nil || body != nil {
		t.Fatalf("user-mode Machine got an edge: %+v", st)
	}
}

func TestApplyEdgeFeedsDropMetrics(t *testing.T) {
	packets := 5
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/vm-1/network/drops" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"items":[{"reason":"dns_deny","policyName":"web-egress","direction":"egress","srcIP":"10.0.0.8","dstIP":"10.0.0.1","proto":"udp","dstPort":53,"packets":%d}]}`, packets)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer fs.Close()
	m := model.Machine{
		Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"},
		Spec:     model.MachineSpec{Network: model.NetworkSpec{DataplaneMode: "ebpf"}},
	}
	rec := metrics.NewNodeRecorder()
	a := &Agent{Flux: fluxvm.New(fs.URL, ""), Metrics: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	counter := rec.Edge().Drops.WithLabelValues("demo", "web", "dns_deny", "web-egress")
	for _, want := range []float64{5, 5, 8} {
		if want == 8 {
			packets = 8
		}
		if _, err := a.applyEdge(context.Background(), m, "vm-1", "10.0.0.8", ""); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(counter); got != want {
			t.Fatalf("kairon_net_drops_total = %v, want %v", got, want)
		}
	}
}

func TestDropDeltasHandleResetAndEviction(t *testing.T) {
	var c edgeCache
	if d := c.dropDeltas("a/", map[string]uint64{"a/x": 10}); d["a/x"] != 10 {
		t.Fatalf("first %v", d)
	}
	if d := c.dropDeltas("a/", map[string]uint64{"a/x": 4}); d["a/x"] != 4 {
		t.Fatalf("after reset %v", d)
	}
	c.dropDeltas("b/", map[string]uint64{"b/y": 1})
	c.dropDeltas("a/", map[string]uint64{})
	if d := c.dropDeltas("a/", map[string]uint64{"a/x": 4}); d["a/x"] != 4 {
		t.Fatal("evicted series baseline kept")
	}
	if d := c.dropDeltas("b/", map[string]uint64{"b/y": 1}); len(d) != 0 {
		t.Fatal("other Machine's series dropped")
	}
}

func TestReconcilePrunesEdgeDropsOnlyAfterSuccessfulList(t *testing.T) {
	for _, failList := range []bool{false, true} {
		t.Run(fmt.Sprintf("failList=%v", failList), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/apis/kairon.zyvor.dev/v1/machines" {
					if failList {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write([]byte(`{"items":[]}`))
					return
				}
				http.NotFound(w, r)
			}))
			defer s.Close()
			kc, err := kube.New(s.URL, "", "", false)
			if err != nil {
				t.Fatal(err)
			}
			defer kc.HTTP.CloseIdleConnections()
			a := &Agent{Kube: kc, NodeName: "worker-1", SysRoot: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			a.edge.dropDeltas("demo/deleted/", map[string]uint64{"demo/deleted/flow": 5})
			err = a.Reconcile(context.Background())
			if (err != nil) != failList {
				t.Fatalf("reconcile = %v", err)
			}
			got := a.edge.dropDeltas("demo/deleted/", map[string]uint64{"demo/deleted/flow": 5})
			if failList && len(got) != 0 {
				t.Fatal("failed list discarded counter baseline")
			}
			if !failList && got["demo/deleted/flow"] != 5 {
				t.Fatal("deleted Machine baseline retained")
			}
		})
	}
}

func TestObserveEdgeDropsBoundsReportedSeries(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms/vm-1/network/drops" {
			_, _ = w.Write([]byte(`{"items":[`))
			for i := 0; i < edgeDropLimit+50; i++ {
				if i > 0 {
					_, _ = w.Write([]byte(","))
				}
				_, _ = fmt.Fprintf(w, `{"reason":"dns_deny","policyName":"web-egress","dstPort":%d,"packets":1}`, i+1)
			}
			_, _ = w.Write([]byte(`]}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	rec := metrics.NewNodeRecorder()
	a := &Agent{Flux: fluxvm.New(s.URL, ""), Metrics: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m := model.Machine{Metadata: model.ObjectMeta{Namespace: "demo", Name: "web"}}
	a.observeEdgeDrops(context.Background(), m, "vm-1")
	counter := rec.Edge().Drops.WithLabelValues("demo", "web", "dns_deny", "web-egress")
	if got := testutil.ToFloat64(counter); got != edgeDropLimit {
		t.Fatalf("counted %v series, want %d", got, edgeDropLimit)
	}
}
