// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

func TestStepClaimBindAndExpire(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	claim := model.MachineClaim{
		Metadata: model.ObjectMeta{Name: "job-1", Namespace: "ml", Annotations: map[string]string{AnnSnapshotOnRelease: "true"}},
		Spec: model.MachineClaimSpec{
			PoolName: "agents", TTLSeconds: 60, ReclaimPolicy: model.ReclaimRetain,
			Egress: &model.ClaimEgress{AllowFqdns: []string{"registry.internal"}},
		},
	}
	decision, err := StepClaim(now, claim, []WarmMachine{{Name: "warm-a", Phase: "Running", PoolState: model.PoolStateWarm, Hypervisor: "firecracker"}})
	if err != nil || decision.Action != ActionBind || decision.Machine != "warm-a" || decision.PolicyName == "" {
		t.Fatalf("%#v %v", decision, err)
	}
	bound := now.Add(-2 * time.Minute)
	claim.Status = model.MachineClaimStatus{Phase: model.ClaimBound, MachineName: "warm-a", BoundAt: &bound}
	decision, err = StepClaim(now, claim, nil)
	if err != nil || decision.Action != ActionExpire || !decision.Snapshot || decision.Reclaim != model.ReclaimRetain {
		t.Fatalf("%#v %v", decision, err)
	}
	open := model.MachineClaim{Metadata: model.ObjectMeta{Name: "job-2"}, Spec: model.MachineClaimSpec{PoolName: "agents", Egress: &model.ClaimEgress{}}}
	if _, err := StepClaim(now, open, nil); err == nil {
		t.Fatal("empty egress should fail")
	}
}

func TestAuditLogAndEvents(t *testing.T) {
	var log Log
	ev, err := log.Append(Event{Principal: "hermes", Tool: "step_agent_claim", Claim: "job-1", At: time.Unix(1, 0).UTC()})
	if err != nil || ev.ID == "" {
		t.Fatal(err)
	}
	if len(log.Replay("job-1")) != 1 || len(log.Replay("other")) != 0 {
		t.Fatal("replay")
	}
	events := EventsFromFindings("job-1", []Finding{{Kind: "beacon", Summary: "regular"}})
	if events[0].Type != "Warning" || events[0].Involved != "job-1" {
		t.Fatalf("%#v", events)
	}
	up, err := CPULabelUpdate(NodeCPUReport{Node: "n1", Effective: "0-3", Reserved: "0", CurrentLabel: "1-3"})
	if err != nil || up.Changed {
		t.Fatalf("%#v %v", up, err)
	}
	st := ProjectConfidential("sev-snp", Attestation{Kind: "tdx", NodeCapable: true, ReportValid: true})
	if st.Sealed {
		t.Fatal("mismatched confidential should not seal")
	}
}

func TestAgentHTTP(t *testing.T) {
	h := Handler()
	body := bytes.NewBufferString(`{"name":"agents","allowFqdns":["registry.internal"]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/compile-policy", body)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "registry.internal") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agent/compile-policy", bytes.NewBufferString(`{"name":"open","allowFqdns":["*"]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wildcard status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agent/anomalies", bytes.NewBufferString(`{"machine":"job-1","flows":[{"dstIP":"10.1.1.1","bytes":20,"intervalSec":10},{"dstIP":"10.1.1.1","bytes":20,"intervalSec":10},{"dstIP":"10.1.1.1","bytes":20,"intervalSec":10}]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "EdgeAnomaly") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
}
