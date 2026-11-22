// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"strings"
	"testing"
	"time"
)

func TestCompilePolicyRejectsOpenAllowlist(t *testing.T) {
	_, err := CompilePolicy(PolicyIntent{Name: "open", AllowFQDNs: []string{"*"}})
	if err == nil {
		t.Fatal("expected wildcard rejection")
	}
	_, err = CompilePolicy(PolicyIntent{Name: "empty"})
	if err == nil || !strings.Contains(err.Error(), "empty allowlist") {
		t.Fatalf("empty allowlist: %v", err)
	}
	_, err = CompilePolicy(PolicyIntent{Name: "world", AllowCIDRs: []string{"0.0.0.0/0"}})
	if err == nil {
		t.Fatal("expected world CIDR rejection")
	}
	pol, err := CompilePolicy(PolicyIntent{
		Name: "agents", Namespace: "ml", Tenant: "acme", Machine: "job-1",
		AllowFQDNs: []string{"registry.internal"}, AllowSNI: []string{"*.internal"}, AllowPorts: []string{"443"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pol.Spec.Policy.DefaultAllow {
		t.Fatal("default allow must stay false")
	}
	if pol.Metadata.Labels["kairon.zyvor.dev/tenant"] != "acme" {
		t.Fatalf("tenant label: %#v", pol.Metadata.Labels)
	}
	if len(pol.Spec.Policy.AllowDNS) != 1 || pol.Spec.Policy.AllowDNS[0] != "registry.internal" {
		t.Fatalf("dns allow: %#v", pol.Spec.Policy.AllowDNS)
	}
}

func TestTenantScope(t *testing.T) {
	if err := CheckTenant(Principal{}, "default", ""); err != nil {
		t.Fatal(err)
	}
	p := Principal{Tenant: "acme", Namespaces: []string{"ml"}}
	if err := CheckTenant(p, "ml", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := CheckTenant(p, "ml", "other"); err == nil {
		t.Fatal("cross-tenant should fail")
	}
	if err := CheckTenant(p, "other", "acme"); err == nil {
		t.Fatal("namespace scope should fail")
	}
}

func TestSealedClaim(t *testing.T) {
	ok := ClaimRequest{
		Pool: "agents", Name: "job-1", Tenant: "acme", TTLSec: 600, Hypervisor: "firecracker",
		Egress: PolicyIntent{Name: "job-1", AllowFQDNs: []string{"pypi.org"}},
		Tools:  []string{"explain_drops"},
	}
	if err := ValidateClaim(ok); err != nil {
		t.Fatal(err)
	}
	bad := ok
	bad.Tools = []string{"delete_machine"}
	if err := ValidateClaim(bad); err == nil {
		t.Fatal("guest delete_machine should be refused")
	}
	bad = ok
	bad.TTLSec = 5
	if err := ValidateClaim(bad); err == nil {
		t.Fatal("short ttl should be refused")
	}
	bad = ok
	bad.Egress = PolicyIntent{Name: "open"}
	if err := ValidateClaim(bad); err == nil {
		t.Fatal("missing egress should be refused")
	}
}

func TestExplainDropsNeverApplies(t *testing.T) {
	got := ExplainDrops([]Drop{{Reason: "sni_deny", SNI: "evil.example", Count: 3, Process: "curl"}})
	if len(got) != 1 || got[0].Apply || !strings.Contains(got[0].Propose[0], "evil.example") {
		t.Fatalf("%#v", got)
	}
}

func TestPlacementAndAnomaly(t *testing.T) {
	ex := ExplainPending("web", []NodeFit{{Name: "n1", Reasons: []string{"storage-domain"}}, {Name: "n2"}})
	if !ex.Pending || !strings.Contains(ex.Summary, "1 of 2") {
		t.Fatalf("%#v", ex)
	}
	flows := []Flow{
		{DstIP: "10.0.0.9", Bytes: 40, IntervalSec: 30},
		{DstIP: "10.0.0.9", Bytes: 40, IntervalSec: 30},
		{DstIP: "10.0.0.9", Bytes: 40, IntervalSec: 31},
		{DNS: strings.Repeat("a", 90) + ".example"},
	}
	findings := Detect(flows, []Drop{{Reason: "dns_deny", Count: 20}})
	kinds := map[string]bool{}
	for _, f := range findings {
		if f.Apply {
			t.Fatal("finding applied")
		}
		kinds[f.Kind] = true
	}
	for _, want := range []string{"beacon", "dns_tunnel_shape", "deny_burst"} {
		if !kinds[want] {
			t.Fatalf("missing %s in %#v", want, findings)
		}
	}
}

func TestConfidentialGPUImageCPUGateway(t *testing.T) {
	if err := AdmitConfidential("sev-snp", Attestation{Kind: "sev-snp", ReportValid: true, NodeCapable: true}); err != nil {
		t.Fatal(err)
	}
	if err := AdmitConfidential("tdx", Attestation{Kind: "sev-snp", NodeCapable: true, ReportValid: true}); err == nil {
		t.Fatal("mismatched attestation should fail")
	}
	if err := AdmitGPU(GPUClaim{Count: 1, LiveMigrate: true}); err == nil {
		t.Fatal("gpu live migration should fail")
	}
	if err := AdmitImage(true, "sha256:"+strings.Repeat("ab", 32), "cosign:sha256:"+strings.Repeat("cd", 32)); err != nil {
		t.Fatal(err)
	}
	if err := AdmitImage(true, "sha256:abcd", "nope"); err == nil {
		t.Fatal("unsigned agent image should fail")
	}
	label, err := ProjectPinnable("0-3", "0-1")
	if err != nil || label != "2-3" {
		t.Fatalf("pinnable %q err %v", label, err)
	}
	gw, err := BindGateway("agents", []PortForward{{GuestPort: 22, HostPort: 2201}})
	if err != nil || gw.Ports[0].Protocol != "tcp" {
		t.Fatal(err)
	}
}

func TestAuditRepairMatrixAdmit(t *testing.T) {
	ev, err := Record(Event{Principal: "hermes", Tool: "compile_network_policy", Claim: "job-1", At: time.Unix(10, 0).UTC()})
	if err != nil || ev.ID == "" {
		t.Fatal(err)
	}
	if len(Replay([]Event{ev, {Claim: "other"}}, "job-1")) != 1 {
		t.Fatal("replay filter")
	}
	steps := ProposeRepair([]BootFinding{{Code: "windows_virtio", Detail: "disk0"}})
	if steps[0].Apply || !strings.Contains(steps[0].Action, "virtio-win") {
		t.Fatalf("%#v", steps)
	}
	if err := MigrationClaim([]MatrixCase{{Name: "cold", Passed: true}}); err == nil {
		t.Fatal("partial matrix should not be green")
	}
	all := make([]MatrixCase, 0, len(RequiredMigrationCases))
	for _, name := range RequiredMigrationCases {
		all = append(all, MatrixCase{Name: name, Passed: true})
	}
	if err := MigrationClaim(all); err != nil {
		t.Fatal(err)
	}
	err = AdmitMachine(MachineAdmission{
		Namespace: "ml", Name: "job-1", Tenant: "acme",
		ImageDigest: "sha256:" + strings.Repeat("ab", 32),
		Annotations: map[string]string{
			AnnAgentPool: "true",
			AnnImageSign: "cosign:sha256:" + strings.Repeat("cd", 32),
			AnnEgress:    "registry.internal",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AdmitMachine(MachineAdmission{Annotations: map[string]string{AnnAgentPool: "true"}}); err == nil {
		t.Fatal("opted-in pool without tenant should fail")
	}
}
