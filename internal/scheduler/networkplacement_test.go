// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/ebpfedge"
	"github.com/zyvorai/kairon/internal/model"
)

func networkFixture(now time.Time) ([]model.Node, []model.Machine, NetworkTelemetry) {
	nodes := []model.Node{rebalanceNodeFixture("a"), rebalanceNodeFixture("b")}
	for i := range nodes {
		n := &nodes[i]
		n.Metadata.Labels["kairon.zyvor.dev/storage-domain"] = "shared"
		n.Metadata.Labels["kairon.zyvor.dev/network-domain"] = "shared"
		n.Metadata.Labels["zone"] = n.Metadata.Name
		n.Metadata.Annotations = map[string]string{model.AnnotationNodePressure: (model.NodePressure{SampledAt: now.Format(time.RFC3339), Host: model.PSI{CPU: 1}}).Encode()}
	}
	machines := []model.Machine{runningOn("db", "a", "2", "2Gi"), runningOn("web", "b", "2", "2Gi")}
	for i := range machines {
		machines[i].Spec.Runtime.Backend = "qemu"
		machines[i].Metadata.UID = machines[i].Metadata.Name + "-uid"
	}
	telemetry := NetworkTelemetry{APIVersion: "networkplacement.kairon.zyvor.dev/v1alpha1", CapturedAt: now, WindowSeconds: 30, SameNodeRTTMillis: 0.1, Edges: []TrafficEdge{{From: "prod/web", To: "prod/db", BytesPerSecond: 1000000}}, Links: []MeasuredLink{{From: "a", To: "b", RTTMillis: 10, SampledAt: now}}}
	return nodes, machines, telemetry
}
func TestNetworkPlacementRecommendation(t *testing.T) {
	now := time.Now().UTC()
	nodes, machines, telemetry := networkFixture(now)
	before, _ := json.Marshal(machines)
	out, err := (Scheduler{RequireCapableLabel: true}).PlanNetworkPlacement(nodes, machines, telemetry, NetworkPlacementOptions{MaxMoves: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 1 || out.Moves[0].Machine != "db" || out.Moves[0].To != "b" || !out.Moves[0].RequiresMigrationPreflight || out.After >= out.Before {
		t.Fatalf("bad plan %+v", out)
	}
	after, _ := json.Marshal(machines)
	if string(before) != string(after) {
		t.Fatal("planner mutated Machine placements")
	}
	if dir := os.Getenv("KAIRON_OPERATIONS_EVIDENCE"); dir != "" {
		b, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "network-placement.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func TestNetworkPlacementRetainsHardGuards(t *testing.T) {
	for _, test := range []string{"capacity", "anti-affinity", "spread", "unknown-pressure", "stale-pressure", "hot", "cordoned", "domain", "pinning", "busy", "cooldown", "migration-cost"} {
		t.Run(test, func(t *testing.T) {
			now := time.Now().UTC()
			nodes, machines, telemetry := networkFixture(now)
			options := NetworkPlacementOptions{}
			for i := range nodes {
				switch test {
				case "capacity":
					nodes[i].Status.Allocatable = map[string]string{"cpu": "2", "memory": "2Gi"}
				case "unknown-pressure":
					nodes[i].Metadata.Annotations = nil
				case "stale-pressure":
					nodes[i].Metadata.Annotations[model.AnnotationNodePressure] = (model.NodePressure{SampledAt: now.Add(-time.Hour).Format(time.RFC3339), Host: model.PSI{CPU: 1}}).Encode()
				case "hot":
					nodes[i].Metadata.Annotations[model.AnnotationNodePressure] = (model.NodePressure{SampledAt: now.Format(time.RFC3339), Host: model.PSI{CPU: 80}}).Encode()
				case "cordoned":
					nodes[i].Spec.Unschedulable = true
				case "domain":
					delete(nodes[i].Metadata.Labels, "kairon.zyvor.dev/storage-domain")
				}
			}
			for i := range machines {
				m := &machines[i]
				switch test {
				case "pinning":
					m.Spec.Resources.CPUPinning = true
				case "cooldown":
					m.Metadata.Annotations = map[string]string{model.AnnotationDRSMovedAt: now.Format(time.RFC3339)}
				case "anti-affinity":
					m.Metadata.Labels = map[string]string{"app": "pair"}
					m.Spec.Placement.AntiAffinity = []model.MachineAffinityTerm{{LabelSelector: map[string]string{"app": "pair"}, TopologyKey: "zone"}}
				case "spread":
					m.Metadata.Labels = map[string]string{"app": "pair"}
					m.Spec.Placement.TopologySpreadConstraints = []model.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "zone", WhenUnsatisfiable: "DoNotSchedule", LabelSelector: map[string]string{"app": "pair"}}}
				}
			}
			if test == "busy" {
				options.Busy = map[string]bool{"prod/db": true, "prod/web": true}
			}
			if test == "migration-cost" {
				options.MigrationPenalty = 1e12
			}
			out, err := (Scheduler{RequireCapableLabel: true}).PlanNetworkPlacement(nodes, machines, telemetry, options, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Moves) != 0 {
				t.Fatalf("violated %s: %+v", test, out.Moves)
			}
		})
	}
}
func TestNetworkPlacementRejectsBadMeasurements(t *testing.T) {
	for _, test := range []string{"stale", "future", "nan-rate", "negative-rate", "missing-link", "duplicate-link", "unknown-peer", "bad-intra"} {
		t.Run(test, func(t *testing.T) {
			now := time.Now().UTC()
			nodes, machines, telemetry := networkFixture(now)
			switch test {
			case "stale":
				telemetry.CapturedAt = now.Add(-time.Hour)
			case "future":
				telemetry.CapturedAt = now.Add(time.Hour)
			case "nan-rate":
				telemetry.Edges[0].BytesPerSecond = math.NaN()
			case "negative-rate":
				telemetry.Edges[0].BytesPerSecond = -1
			case "missing-link":
				telemetry.Links = nil
			case "duplicate-link":
				telemetry.Links = append(telemetry.Links, telemetry.Links[0])
			case "unknown-peer":
				telemetry.Edges[0].To = "prod/missing"
			case "bad-intra":
				telemetry.SameNodeRTTMillis = 0
			}
			if _, err := (Scheduler{}).PlanNetworkPlacement(nodes, machines, telemetry, NetworkPlacementOptions{}, now); err == nil {
				t.Fatalf("accepted %s", test)
			}
		})
	}
}
func TestNetworkPlacementMissingCandidateRTTDoesNotGuess(t *testing.T) {
	now := time.Now().UTC()
	nodes, machines, telemetry := networkFixture(now)
	c := nodes[0]
	c.Metadata = model.ObjectMeta{Name: "c", Labels: map[string]string{model.CapableLabel: "true", "allow": "c", "kairon.zyvor.dev/storage-domain": "shared", "kairon.zyvor.dev/network-domain": "shared"}, Annotations: nodes[0].Metadata.Annotations}
	nodes = append(nodes, c)
	for i := range machines {
		machines[i].Spec.Placement.NodeSelector = map[string]string{"allow": "c"}
	}
	out, err := (Scheduler{}).PlanNetworkPlacement(nodes, machines, telemetry, NetworkPlacementOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Moves) != 0 || out.Rejected["missing candidate RTT measurement"] == 0 {
		t.Fatalf("guessed RTT: %+v", out)
	}
	if !strings.Contains(out.Model, "estimates") {
		t.Fatal("report claims measured application latency")
	}
}

func TestTrafficCollectionUsesDeltasNotLifetimeCounters(t *testing.T) {
	now := time.Now().UTC()
	_, machines, _ := networkFixture(now)
	machines[0].Status.GuestIP = "10.0.0.2"
	machines[1].Status.GuestIP = "10.0.0.3"
	f := ebpfedge.FlowRecord{FlowKey: ebpfedge.FlowKey{SrcIP: "10.0.0.3", DstIP: "10.0.0.2", Proto: "tcp", SrcPort: 1000, DstPort: 5432}, Bytes: 900000, FirstSeen: now.Add(-time.Hour)}
	before := map[string][]ebpfedge.FlowRecord{"prod/web": {f}}
	f.Bytes += 3000
	after := map[string][]ebpfedge.FlowRecord{"prod/web": {f}, "prod/db": {f}}
	edges, stats, err := TrafficFromCounters(machines, before, after, now, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].BytesPerSecond != 1000 || stats.Reset != 0 {
		t.Fatal(edges, stats)
	}
	after["prod/web"][0].Bytes = 1
	edges, stats, err = TrafficFromCounters(machines, before, after, now, now.Add(3*time.Second))
	if err != nil || len(edges) != 0 || stats.Reset != 1 {
		t.Fatal(edges, stats, err)
	}
	after["prod/web"][0].Bytes = 1000000
	after["prod/web"][0].FirstSeen = now.Add(time.Second)
	edges, stats, err = TrafficFromCounters(machines, before, after, now, now.Add(3*time.Second))
	if err != nil || len(edges) != 0 || stats.Reset != 1 {
		t.Fatal("recreated flow counter was treated as a monotonic continuation", edges, stats, err)
	}
	machines[1].Status.GuestIP = machines[0].Status.GuestIP
	if _, _, err := TrafficFromCounters(machines, before, after, now, now.Add(time.Second)); err == nil {
		t.Fatal("ambiguous IP accepted")
	}
}
func TestMeasuredFlowSchema(t *testing.T) {
	good := json.RawMessage(`{"items":[{"bytes":100,"srcIP":"10.0.0.1","dstIP":"10.0.0.2"}]}`)
	if flows, err := DecodeMeasuredFlows(good); err != nil || len(flows) != 1 {
		t.Fatal(flows, err)
	}
	for _, bad := range []string{`{}`, `{"items":[{"dstIP":"10.0.0.1"}]}`, `{"items":[{"bytes":-1,"srcIP":"a","dstIP":"b"}]}`} {
		if _, err := DecodeMeasuredFlows(json.RawMessage(bad)); err == nil {
			t.Fatal("unsupported flow schema accepted")
		}
	}
}
