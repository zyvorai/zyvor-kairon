// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/attest"
	"github.com/zyvorai/kairon/internal/model"
)

// fakeAttest accepts a report whose bytes are exactly the expected
// REPORT_DATA, so the test exercises the nonce binding end to end.
type fakeAttest struct{ calls int }

func (f *fakeAttest) Verify(_ context.Context, kind string, raw []byte, want [64]byte) error {
	f.calls++
	if !bytes.Equal(raw, want[:]) {
		return errors.New(kind + " report_data does not match this Machine's nonce")
	}
	return nil
}

func confidentialMachine(ann map[string]string) model.Machine {
	a := map[string]string{agentplane.AnnConfidential: "sev-snp"}
	for k, v := range ann {
		a[k] = v
	}
	return model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod", UID: "uid-1", Annotations: a}, Spec: model.MachineSpec{NodeName: "n1"}}
}

func TestAttestationRound(t *testing.T) {
	v := &fakeAttest{}
	c := &Controller{Attest: v, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	p := c.attestationStep(ctx, confidentialMachine(nil), "sev-snp")
	nonce, _ := p[agentplane.AnnAttestationNonce].(string)
	if len(nonce) != 64 {
		t.Fatalf("first step must issue a nonce: %v", p)
	}
	if p := c.attestationStep(ctx, confidentialMachine(map[string]string{agentplane.AnnAttestationNonce: nonce}), "sev-snp"); p != nil {
		t.Fatalf("no report yet must wait: %v", p)
	}

	good := attest.ReportData("uid-1", nonce)
	p = c.attestationStep(ctx, confidentialMachine(map[string]string{
		agentplane.AnnAttestationNonce:  nonce,
		agentplane.AnnAttestationReport: base64.StdEncoding.EncodeToString(good[:]),
	}), "sev-snp")
	if p[agentplane.AnnAttestationVerified] != "sev-snp" || p[agentplane.AnnAttestationNonce] != nil || p[agentplane.AnnAttestationReport] != nil {
		t.Fatalf("valid report: %v", p)
	}

	other := attest.ReportData("uid-2", nonce)
	p = c.attestationStep(ctx, confidentialMachine(map[string]string{
		agentplane.AnnAttestationNonce:  nonce,
		agentplane.AnnAttestationReport: base64.StdEncoding.EncodeToString(other[:]),
	}), "sev-snp")
	if p[agentplane.AnnAttestationVerified] != nil || !strings.Contains(p[agentplane.AnnAttestationError].(string), "report_data") || p[agentplane.AnnAttestationNonce] == nonce {
		t.Fatalf("another Machine's report must fail and rotate the nonce: %v", p)
	}

	if p := c.attestationStep(ctx, confidentialMachine(map[string]string{agentplane.AnnAttestationVerified: "sev-snp"}), ""); p[agentplane.AnnAttestationVerified] != nil || p[agentplane.AnnAttestationError] == nil {
		t.Fatalf("losing the node capability must unseal: %v", p)
	}
	if _, ok := c.attestationStep(ctx, confidentialMachine(map[string]string{agentplane.AnnAttestationVerified: "sev-snp"}), "")[agentplane.AnnAttestationVerified]; !ok {
		t.Fatal("unseal patch must delete the verified key")
	}
	if p := c.attestationStep(ctx, confidentialMachine(map[string]string{agentplane.AnnAttestationVerified: "sev-snp"}), "sev-snp"); p != nil {
		t.Fatalf("already verified: %v", p)
	}
	if p := c.attestationStep(ctx, confidentialMachine(nil), "tdx"); p != nil {
		t.Fatalf("wrong node kind must not issue a nonce: %v", p)
	}
}

func TestReconcileAttestationPatchesMachine(t *testing.T) {
	c, fake := newPoolTestController(t)
	c.Attest = &fakeAttest{}
	node := model.Node{Metadata: model.ObjectMeta{Name: "n1", Labels: map[string]string{agentplane.LabelConfidentialCapable: "sev-snp"}}}
	c.reconcileAttestation(t.Context(), []model.Machine{confidentialMachine(nil)}, []model.Node{node})
	a, _ := fake.machinePatch["m1"]["annotations"].(map[string]any)
	if n, _ := a[agentplane.AnnAttestationNonce].(string); len(n) != 64 {
		t.Fatalf("patch = %v", fake.machinePatch["m1"])
	}

	c.Attest = nil
	delete(fake.machinePatch, "m1")
	c.reconcileAttestation(t.Context(), []model.Machine{confidentialMachine(nil)}, []model.Node{node})
	if _, ok := fake.machinePatch["m1"]; ok {
		t.Fatal("no verifier must mean no attestation writes")
	}
}

func TestWebhookGuardsAttestationAnnotations(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	forged := model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod", Annotations: map[string]string{agentplane.AnnAttestationVerified: "sev-snp"}}}

	r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, forged)
	req.UserInfo.Username = "alice"
	if d := ctl.validateMachine(r, req); d.Allowed || !strings.Contains(d.Reason, "kairon-controller only") {
		t.Fatalf("create with verified preset: %+v", d)
	}

	plain := model.Machine{Metadata: model.ObjectMeta{Name: "m1", Namespace: "prod"}}
	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, forged, plain)
	req.UserInfo.Username = "alice"
	if d := ctl.validateMachine(r, req); d.Allowed {
		t.Fatal("user set attestation-verified on update")
	}
	req.UserInfo.Username = "system:serviceaccount:kairon-system:kairon-controller"
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("controller denied: %s", d.Reason)
	}

	withReport := plain
	withReport.Metadata.Annotations = map[string]string{agentplane.AnnAttestationReport: "AAAA"}
	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, withReport, plain)
	req.UserInfo.Username = "alice"
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("report is unguarded (it is verified): %s", d.Reason)
	}
	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, plain, forged)
	req.UserInfo.Username = "alice"
	if d := ctl.validateMachine(r, req); !d.Allowed {
		t.Fatalf("removing verified only unseals: %s", d.Reason)
	}

	ctl.AttestationWriters = []string{"kairon-admin"}
	r, req = admissionReqWithOld(t, "machines", "prod", admission.OperationUpdate, forged, plain)
	req.UserInfo.Username = "system:serviceaccount:kairon-system:kairon-controller"
	if d := ctl.validateMachine(r, req); d.Allowed {
		t.Fatal("explicit writer list must replace the default")
	}
}
