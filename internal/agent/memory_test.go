// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func u64(v uint64) *uint64 { return &v }

func f64(v float64) *float64 { return &v }

func host(availMiB, totalMiB uint64) fluxvm.HostMemory {
	return fluxvm.HostMemory{AvailableMiB: u64(availMiB), TotalMiB: u64(totalMiB)}
}

func TestWatermarksDefaultToShareOfTotal(t *testing.T) {
	low, high := MemoryReclaimConfig{}.watermarks(32768)
	if low != 3276 || high != 6553 {
		t.Fatalf("low=%d high=%d", low, high)
	}
	low, high = MemoryReclaimConfig{LowWatermarkMiB: 2048}.watermarks(32768)
	if low != 2048 || high != 4096 {
		t.Fatalf("explicit low: low=%d high=%d", low, high)
	}
	low, high = MemoryReclaimConfig{}.watermarks(0)
	if low != 1024 || high != 2048 {
		t.Fatalf("unknown total: low=%d high=%d", low, high)
	}
}

func TestPlanReclaimInflatesInProportionUnderPressure(t *testing.T) {
	// 16Gi host, 1Gi available: low 1638, high 3276, need 2252MiB.
	vms := []balloonVM{
		{Key: "a", CurrentMiB: 8192, MinMiB: 2048},
		{Key: "b", CurrentMiB: 4096, MinMiB: 2048},
	}
	plan := planReclaim(MemoryReclaimConfig{}, host(1024, 16384), vms)
	if !plan.Pressure || plan.Exhausted || plan.Reason != reasonHostPressure {
		t.Fatalf("plan %+v", plan)
	}
	// a: headroom 6144, step 1536, share ceil(2252*6144/8192)=1689 -> 1536.
	// b: headroom 2048, step 512, share 563 -> 512.
	if plan.Targets["a"] != 1536 || plan.Targets["b"] != 512 {
		t.Fatalf("targets %v", plan.Targets)
	}
}

func TestPlanReclaimNeverGoesBelowMin(t *testing.T) {
	vms := []balloonVM{{Key: "a", CurrentMiB: 4096, MinMiB: 2048, TargetMiB: 2000}}
	plan := planReclaim(MemoryReclaimConfig{}, host(100, 16384), vms)
	if plan.Targets["a"] != 2048 {
		t.Fatalf("targets %v", plan.Targets)
	}
	vms[0].TargetMiB = 2048
	plan = planReclaim(MemoryReclaimConfig{}, host(100, 16384), vms)
	if !plan.Exhausted || len(plan.Targets) != 0 || !strings.Contains(plan.Message, "minimum") {
		t.Fatalf("plan %+v", plan)
	}
}

func TestPlanReclaimWithNoElasticMachinesIsExhaustedUnderPressure(t *testing.T) {
	plan := planReclaim(MemoryReclaimConfig{}, host(100, 16384), nil)
	if !plan.Pressure || !plan.Exhausted {
		t.Fatalf("plan %+v", plan)
	}
	plan = planReclaim(MemoryReclaimConfig{}, host(8192, 16384), nil)
	if plan.Pressure || plan.Exhausted {
		t.Fatalf("calm host: %+v", plan)
	}
}

func TestPlanReclaimPSIAlonePressuresOneStepEach(t *testing.T) {
	h := host(12000, 16384)
	h.PSISomeAvg10 = f64(25)
	plan := planReclaim(MemoryReclaimConfig{}, h, []balloonVM{{Key: "a", CurrentMiB: 4096, MinMiB: 1024}})
	if !plan.Pressure || plan.Targets["a"] != 768 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestPlanReclaimDeflatesWithinBudget(t *testing.T) {
	// 16Gi host, 3500 available: high 3276, budget 224MiB.
	vms := []balloonVM{
		{Key: "a", CurrentMiB: 8192, MinMiB: 2048, TargetMiB: 1000},
		{Key: "b", CurrentMiB: 4096, MinMiB: 2048, TargetMiB: 500},
	}
	plan := planReclaim(MemoryReclaimConfig{}, host(3500, 16384), vms)
	if plan.Pressure || plan.Reason != reasonRelief {
		t.Fatalf("plan %+v", plan)
	}
	if plan.Targets["a"] != 776 {
		t.Fatalf("targets %v", plan.Targets)
	}
	if _, ok := plan.Targets["b"]; ok {
		t.Fatalf("budget exhausted on a, b must hold: %v", plan.Targets)
	}
}

func TestPlanReclaimHoldsBetweenWatermarks(t *testing.T) {
	vms := []balloonVM{{Key: "a", CurrentMiB: 8192, MinMiB: 2048, TargetMiB: 1000}}
	plan := planReclaim(MemoryReclaimConfig{}, host(2500, 16384), vms)
	if plan.Pressure || len(plan.Targets) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	h := host(9000, 16384)
	h.PSISomeAvg10 = f64(7)
	if plan := planReclaim(MemoryReclaimConfig{}, h, vms); len(plan.Targets) != 0 {
		t.Fatalf("warm PSI must not deflate: %+v", plan)
	}
}

func TestReclaimMemoryPassInflatesAndRecordsStatus(t *testing.T) {
	var mu sync.Mutex
	var balloonSets []string
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/runtime/capabilities":
			_, _ = io.WriteString(w, `{"host":{"topology":true,"numaBinding":true,"pciBind":true,"balloon":true}}`)
		case r.URL.Path == "/v1/host/memory":
			_, _ = io.WriteString(w, `{"mem_total_mib":16384,"mem_available_mib":1024,"psi_some_avg10":1.0,"psi_full_avg10":0}`)
		case r.URL.Path == "/v1/vms/vm-1/memory":
			_, _ = io.WriteString(w, `{"configured_mib":4096,"balloon":{"memory_mib":4096,"target_mib":0,"actual_mib":0},"guest":{"available_mib":2500,"last_update":1}}`)
		case r.URL.Path == "/v1/vms/vm-1/balloon" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			balloonSets = append(balloonSets, string(b))
			mu.Unlock()
			_, _ = io.WriteString(w, `{"memory_mib":4096,"target_mib":768,"actual_mib":300}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fs.Close()
	var statusPatch, nodePatch map[string]any
	var nodeContentType string
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/machines/m1/status"):
			_ = json.Unmarshal(body, &statusPatch)
		case r.URL.Path == "/api/v1/nodes/n1/status":
			nodeContentType = r.Header.Get("Content-Type")
			_ = json.Unmarshal(body, &nodePatch)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	a := &Agent{NodeName: "n1", Kube: kc, Flux: fluxvm.New(fs.URL, "")}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}}
	m.Spec.NodeName = "n1"
	m.Spec.Resources.Memory = "4Gi"
	m.Spec.Resources.ElasticMemory = &model.ElasticMemorySpec{Min: "1Gi"}
	m.Status.Phase = "Running"
	m.Status.RuntimeID = "vm-1"
	a.memory.setMachines([]model.Machine{m})

	a.reclaimMemory(t.Context())

	if len(balloonSets) != 1 || !strings.Contains(balloonSets[0], `"balloon_mib":768`) {
		t.Fatalf("balloon sets %v", balloonSets)
	}
	mem, _ := statusPatch["status"].(map[string]any)["memory"].(map[string]any)
	if mem["balloonTargetMiB"] != float64(768) || mem["balloonActualMiB"] != float64(300) || mem["reason"] != reasonHostPressure || mem["guestAvailableMiB"] != float64(2500) {
		t.Fatalf("status patch %v", statusPatch)
	}
	if nodeContentType != "application/strategic-merge-patch+json" {
		t.Fatalf("node condition must use a strategic merge patch, got %q", nodeContentType)
	}
	conds := nodePatch["status"].(map[string]any)["conditions"].([]any)
	c := conds[0].(map[string]any)
	if c["type"] != model.NodeConditionKaironMemoryPressure || c["status"] != "False" {
		t.Fatalf("node condition %v", c)
	}

	// Not due again until the interval passes: no second call.
	a.reclaimMemory(t.Context())
	if len(balloonSets) != 1 {
		t.Fatalf("second pass inside the interval moved the balloon: %v", balloonSets)
	}
}

func TestWriteMemoryStatusUsesOwnWritesOverStaleList(t *testing.T) {
	var patches []map[string]any
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		patches = append(patches, p)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer ks.Close()
	kc, _ := kube.New(ks.URL, "", "", false)
	a := &Agent{NodeName: "n1", Kube: kc}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "default"}}
	// The listed Machine predates the inflate below.
	m.Status.Memory = &model.MemoryStatus{CurrentMiB: 4096, MinMiB: 1024}
	e := elasticMachine{m: m, report: &fluxvm.VMMemoryReport{}, vm: balloonVM{Key: "default/m1", CurrentMiB: 4096, MinMiB: 1024}}

	a.writeMemoryStatus(t.Context(), e, 768, 700, reasonHostPressure)
	a.writeMemoryStatus(t.Context(), e, 768, 710, "")
	if len(patches) != 1 {
		t.Fatalf("an unchanged target with the list still stale must not rewrite: %d patches", len(patches))
	}

	// FluxVM now reports target 0 on its own: the VM was relaunched.
	a.writeMemoryStatus(t.Context(), e, 0, 0, "")
	if len(patches) != 2 {
		t.Fatalf("patches %d", len(patches))
	}
	mem := patches[1]["status"].(map[string]any)["memory"].(map[string]any)
	if mem["reason"] != reasonRestart {
		t.Fatalf("reason %v", mem["reason"])
	}
}
