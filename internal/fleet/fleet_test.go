// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fleet

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func object(kind string, spec any) model.FleetResource {
	return model.FleetResource{TypeMeta: model.TypeMeta{APIVersion: model.FleetAPIVersion, Kind: kind}, Metadata: model.ObjectMeta{Name: "sample", Namespace: "default", UID: "owner-uid", Generation: 1, ResourceVersion: "1"}, Spec: raw(spec)}
}
func clientFor(t *testing.T, handler http.HandlerFunc) *kube.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := kube.New(server.URL, "test-token", "", false)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestValidationRejectsUnsafeInputs(t *testing.T) {
	cases := []model.FleetResource{
		object("MachineHAProfile", HAProfileSpec{Nodes: map[string]RedfishTarget{"host": {Endpoint: "http://bmc", SecretName: "secret", SystemID: "system"}}, MaxRestarts: 1, FailureGraceSeconds: 30}),
		object("MachineAutoscaler", AutoscalerSpec{TargetKind: "MachineSet", TargetName: "workers", MinReplicas: 0, MaxReplicas: 3, TargetCPUPercent: 60, StabilizationSeconds: 30, MaxStep: 1}),
		object("MachineBalancePolicy", BalanceSpec{Selector: map[string]string{"app": "api"}, MinImprovementPercent: 10, CooldownSeconds: 30, Strategy: "live"}),
		object("MachineVirtualNetwork", VirtualNetworkSpec{CIDR: "10.0.0.1/24", Gateway: "10.0.0.1", Bridge: "br-a", NodeSelector: map[string]string{"zone": "a"}}),
		object("MachineTemplateVersion", TemplateSpec{Version: "1", MaxTTLSeconds: 60, Template: model.MachineTemplate{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "1", Memory: "1Gi"}}}}),
		object("MachineBackupGroup", BackupGroupSpec{Machines: []string{"same", "same"}, Quiesce: "required"}),
		object("MachineNetworkClaim", NetworkClaimSpec{NetworkName: "../../secrets", MachineName: "api"}),
		object("MachineUsageLedger", LedgerSpec{MaxGapSeconds: 3601}),
	}
	for _, o := range cases {
		t.Run(o.Kind, func(t *testing.T) {
			if Validate(o) == nil {
				t.Fatal("unsafe spec accepted")
			}
		})
	}
	valid := object("MachineAutoscaler", AutoscalerSpec{TargetKind: "MachineSet", TargetName: "workers", MinReplicas: 1, MaxReplicas: 3, TargetCPUPercent: 60, StabilizationSeconds: 30, MaxStep: 1})
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	valid.Spec = json.RawMessage(`{"targetKind":"MachineSet","targetName":"workers","minReplicas":1,"maxReplicas":3,"targetCPUPercent":60,"stabilizationSeconds":30,"maxStep":1,"typo":true}`)
	if Validate(valid) == nil {
		t.Fatal("unknown field accepted")
	}
}
func TestRecoveryDependencyGraph(t *testing.T) {
	base := RecoverySpec{Mode: "RestoreInPlace", MaxDataAgeSeconds: 3600, Steps: []RecoveryStep{{MachineName: "db", BackupName: "db-b"}, {MachineName: "api", BackupName: "api-b", DependsOn: []string{"db"}}}}
	if err := Validate(object("MachineRecoveryPlan", base)); err != nil {
		t.Fatal(err)
	}
	base.Steps[0].DependsOn = []string{"api"}
	if Validate(object("MachineRecoveryPlan", base)) == nil {
		t.Fatal("cycle accepted")
	}
	base.Steps[0].DependsOn = []string{"missing"}
	if Validate(object("MachineRecoveryPlan", base)) == nil {
		t.Fatal("missing dependency accepted")
	}
}
func TestAllocateAddressIPv4IPv6AndExhaustion(t *testing.T) {
	for _, tc := range []struct{ cidr, gateway, want string }{{"10.0.0.0/30", "10.0.0.1", "10.0.0.2"}, {"fd00::/120", "fd00::1", "fd00::2"}} {
		s := VirtualNetworkSpec{CIDR: tc.cidr, Gateway: tc.gateway}
		address, err := AllocateAddress(s, nil)
		if err != nil || address != tc.want {
			t.Fatalf("%s: %q %v", tc.cidr, address, err)
		}
	}
	_, err := AllocateAddress(VirtualNetworkSpec{CIDR: "10.0.0.0/30", Gateway: "10.0.0.1"}, map[string]string{"claim": "10.0.0.2"})
	if err == nil {
		t.Fatal("broadcast address allocated")
	}
}
func TestNetworkClaimUsesCASAndReusesDurableAllocation(t *testing.T) {
	network := object("MachineVirtualNetwork", VirtualNetworkSpec{CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Bridge: "br-a", NodeSelector: map[string]string{"zone": "a"}})
	network.Metadata.Name = "net-a"
	writes := 0
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/net-a"):
			respond(w, network)
		case r.Method == "GET":
			respond(w, map[string]any{"items": []model.FleetResource{network}})
		case r.Method == "PATCH":
			var patch struct {
				Metadata model.ObjectMeta  `json:"metadata"`
				Status   model.FleetStatus `json:"status"`
			}
			_ = json.NewDecoder(r.Body).Decode(&patch)
			if patch.Metadata.ResourceVersion != "1" {
				t.Error("allocation write has no CAS")
			}
			network.Status = patch.Status
			writes++
			respond(w, network)
		default:
			http.NotFound(w, r)
		}
	})
	e := Engine{Kube: client}
	claim := object("MachineNetworkClaim", NetworkClaimSpec{NetworkName: "net-a", MachineName: "api"})
	claim.Metadata.UID = "claim-a"
	if err := e.networkClaim(context.Background(), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.Status.Allocations["address"] != "10.0.0.2" {
		t.Fatal(claim.Status)
	}
	if err := e.networkClaim(context.Background(), &claim); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("replay allocated twice: %d", writes)
	}
	claim.Metadata.UID = "claim-b"
	if err := e.networkClaim(context.Background(), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.Status.Allocations["address"] != "10.0.0.3" {
		t.Fatal("new UID stole old address")
	}
}
func TestScaleRecommendationBoundsAndSteps(t *testing.T) {
	s := AutoscalerSpec{MinReplicas: 2, MaxReplicas: 10, TargetCPUPercent: 50, MaxStep: 2}
	for _, tc := range []struct {
		current int
		use     float64
		want    int
	}{{4, 100, 6}, {4, 0, 2}, {10, 200, 10}, {2, 1, 2}, {4, 50, 4}} {
		if got := ScaleRecommendation(tc.current, tc.use, s); got != tc.want {
			t.Fatalf("%+v: %d", tc, got)
		}
	}
}
func TestMetricsRejectStaleDuplicateAndNaN(t *testing.T) {
	now := time.Now()
	fresh := float64(now.Unix())
	value := "80"
	duplicates := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("query"), "timestamp") {
			respond(w, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": map[string]string{"machine": "api"}, "value": []any{fresh, fmt.Sprint(fresh)}}}}})
			return
		}
		result := []any{map[string]any{"metric": map[string]string{"machine": "api"}, "value": []any{fresh, value}}}
		if duplicates {
			result = append(result, result[0])
		}
		respond(w, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	defer server.Close()
	p := &Prometheus{URL: server.URL}
	cpu, err := p.CPU(context.Background(), "default", now)
	if err != nil || cpu["api"] != 80 {
		t.Fatalf("fresh sample: %v %v", cpu, err)
	}
	fresh -= 120
	cpu, err = p.CPU(context.Background(), "default", now)
	if err != nil || len(cpu) != 0 {
		t.Fatal("stale sample accepted")
	}
	fresh = float64(now.Unix())
	duplicates = true
	if _, err = p.CPU(context.Background(), "default", now); err == nil {
		t.Fatal("duplicate accepted")
	}
	duplicates = false
	value = "NaN"
	if _, err = p.CPU(context.Background(), "default", now); err == nil {
		t.Fatal("NaN accepted")
	}
}
func TestMeterGapAndReplay(t *testing.T) {
	now := time.Now()
	e := Engine{Now: func() time.Time { return now }}
	o := object("MachineUsageLedger", LedgerSpec{Selector: map[string]string{"team": "eng"}, MaxGapSeconds: 60})
	m := model.Machine{Metadata: model.ObjectMeta{Name: "api", Namespace: "default", Labels: map[string]string{"team": "eng"}}, Spec: model.MachineSpec{NodeName: "host", Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}}, Status: model.MachineStatus{Phase: "Running"}}
	if err := e.meter(context.Background(), &o, []model.Machine{m}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if err := e.meter(context.Background(), &o, []model.Machine{m}); err != nil {
		t.Fatal(err)
	}
	expected := float64(2) * 30 / 3600
	if o.Status.Totals["vcpu_hours"] != expected {
		t.Fatal(o.Status)
	}
	if err := e.meter(context.Background(), &o, []model.Machine{m}); err != nil {
		t.Fatal(err)
	}
	if o.Status.Totals["vcpu_hours"] != expected {
		t.Fatal("replay double-counted")
	}
	now = now.Add(120 * time.Second)
	if err := e.meter(context.Background(), &o, []model.Machine{m}); err != nil {
		t.Fatal(err)
	}
	if o.Status.Totals["vcpu_hours"] != expected || o.Status.Totals["unobserved_seconds"] != 120 {
		t.Fatal("gap was billed")
	}
}
func TestApprovalCanonicalBindingConsumeAndReplay(t *testing.T) {
	now := time.Now()
	h1, _ := ArgumentsHash("delete", json.RawMessage(`{"a":1,"b":2}`), "agent")
	h2, _ := ArgumentsHash("delete", json.RawMessage(`{"b":2,"a":1}`), "agent")
	if h1 != h2 {
		t.Fatal("JSON order changes identity")
	}
	s := ApprovalSpec{Action: "delete", ArgumentsHash: h1, Principal: "agent", TargetResource: "machines", TargetName: "api", TargetUID: "machine-uid", TargetGeneration: 3, Approver: "human", ExpiresAt: now.Add(5 * time.Minute)}
	o := object("MachineActionApproval", s)
	o.Metadata.Name = ApprovalName(s)
	writes := 0
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			respond(w, o)
			return
		}
		var patch struct {
			Metadata model.ObjectMeta  `json:"metadata"`
			Status   model.FleetStatus `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		if patch.Metadata.ResourceVersion != o.Metadata.ResourceVersion {
			http.Error(w, "conflict", http.StatusConflict)
			return
		}
		writes++
		o.Status = patch.Status
		respond(w, o)
	})
	actor, err := ConsumeApproval(context.Background(), c, "default", s, now)
	if err != nil || actor != "human" {
		t.Fatalf("consume: %q %v", actor, err)
	}
	if _, err := ConsumeApproval(context.Background(), c, "default", s, now); err == nil {
		t.Fatal("replayed approval")
	}
	if writes != 1 {
		t.Fatal("replay wrote status")
	}
	o.Status = model.FleetStatus{}
	changed := s
	changed.TargetGeneration++
	if _, err := ConsumeApproval(context.Background(), c, "default", changed, now); err == nil {
		t.Fatal("changed target approved")
	}
	if _, err := ConsumeApproval(context.Background(), c, "default", s, now.Add(6*time.Minute)); err == nil {
		t.Fatal("expired approval accepted")
	}
}
func TestRedfishFencingWaitsForObservedOffAndVerifiesTLS(t *testing.T) {
	power := "On"
	reset := 0
	bmc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "operator" || p != "password" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.Method == "POST" {
			reset++
			w.WriteHeader(202)
			return
		}
		respond(w, map[string]string{"PowerState": power})
	}))
	defer bmc.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bmc.Certificate().Raw})
	if _, err := x509.ParseCertificate(bmc.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
	target := RedfishTarget{Endpoint: bmc.URL, SystemID: "system", SecretName: "bmc-secret"}
	profile := object("MachineHAProfile", HAProfileSpec{Selector: map[string]string{"ha": "yes"}, Nodes: map[string]RedfishTarget{"host": target}, MaxRestarts: 1, FailureGraceSeconds: 30})
	profile.Metadata.Namespace = "kairon-system"
	node := model.Node{Metadata: model.ObjectMeta{Name: "host", UID: "host-uid", ResourceVersion: "1"}}
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/secrets/") {
			respond(w, map[string]any{"data": map[string][]byte{"username": []byte("operator"), "password": []byte("password"), "ca.crt": ca}})
			return
		}
		if strings.Contains(r.URL.Path, "machinehaprofiles") {
			respond(w, profile)
			return
		}
		respond(w, node)
	})
	e := Engine{Kube: c, ControlNamespace: "kairon-system", RedfishOrigins: []string{bmc.URL}}
	o := object("NodeFenceRequest", FenceSpec{Node: "host", NodeUID: "host-uid", ProfileName: "sample"})
	o.Metadata.Namespace = "kairon-system"
	if err := e.fenceNode(context.Background(), &o, []model.Node{node}); err != nil {
		t.Fatal(err)
	}
	if o.Status.Phase != "WaitingForPowerOff" || reset != 1 {
		t.Fatal(o.Status)
	}
	power = "Off"
	if err := e.fenceNode(context.Background(), &o, []model.Node{node}); err != nil {
		t.Fatal(err)
	}
	if o.Status.Phase != "Succeeded" {
		t.Fatal("Off not recorded")
	}
	o.Status = model.FleetStatus{}
	e.RedfishOrigins = nil
	if e.fenceNode(context.Background(), &o, []model.Node{node}) == nil {
		t.Fatal("nonallowlisted origin accepted")
	}
	e.RedfishOrigins = []string{bmc.URL}
	node.Metadata.UID = "new-host"
	if e.fenceNode(context.Background(), &o, []model.Node{node}) == nil {
		t.Fatal("replaced host fenced")
	}
}
