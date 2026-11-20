// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/attest"
	"github.com/zyvorai/kairon/internal/model"
)

// AttestationVerifier checks a report or quote against the 64 bytes it
// must carry in REPORT_DATA. *attest.Verifier implements it.
type AttestationVerifier interface {
	Verify(ctx context.Context, kind string, raw []byte, want [64]byte) error
}

const attestVerifyTimeout = 20 * time.Second

// reconcileAttestation runs one challenge round per Machine that asks
// for confidential compute on a node that claims the matching kind:
// issue a nonce, wait for a report, verify it, and only then set
// attestation-verified. A failed report rotates the nonce, so the next
// report must be fresh.
func (c *Controller) reconcileAttestation(ctx context.Context, machines []model.Machine, nodes []model.Node) {
	if c.Attest == nil {
		return
	}
	capable := make(map[string]string, len(nodes))
	for _, n := range nodes {
		capable[n.Metadata.Name] = n.Metadata.Labels[agentplane.LabelConfidentialCapable]
	}
	for _, m := range machines {
		if patch := c.attestationStep(ctx, m, capable[m.Spec.NodeName]); patch != nil {
			if err := c.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, map[string]any{"metadata": map[string]any{"annotations": patch}}); err != nil {
				c.Log.Error("machine attestation patch failed", "namespace", m.Namespace(), "machine", m.Metadata.Name, "error", err)
			}
		}
	}
}

// guardedAttestationKeys are the annotations only the controller may
// set: verified seals the Machine, and a chosen nonce would let an old
// report be replayed. The report itself is unguarded; it is verified.
var guardedAttestationKeys = []string{agentplane.AnnAttestationVerified, agentplane.AnnAttestationNonce}

// checkAttestationWrite denies a Machine write that sets or changes a
// guarded key unless username is an attestation writer.
func (c *Controller) checkAttestationWrite(username string, oldAnn, newAnn map[string]string) error {
	if c.attestationWriter(username) {
		return nil
	}
	for _, k := range guardedAttestationKeys {
		if oldAnn[k] != newAnn[k] && newAnn[k] != "" {
			return fmt.Errorf("annotation %s is set by kairon-controller only", k)
		}
	}
	return nil
}

func (c *Controller) attestationWriter(username string) bool {
	if len(c.AttestationWriters) == 0 {
		return strings.HasPrefix(username, "system:serviceaccount:") && strings.HasSuffix(username, ":kairon-controller")
	}
	for _, w := range c.AttestationWriters {
		if w == username {
			return true
		}
	}
	return false
}

// attestationStep returns the annotation patch for one Machine, or nil.
func (c *Controller) attestationStep(ctx context.Context, m model.Machine, nodeKind string) map[string]any {
	ann := m.Metadata.Annotations
	kind := strings.ToLower(strings.TrimSpace(ann[agentplane.AnnConfidential]))
	if m.Metadata.DeletionTimestamp != nil || (kind != attest.KindSNP && kind != attest.KindTDX) || m.Spec.NodeName == "" {
		return nil
	}
	verified := ann[agentplane.AnnAttestationVerified]
	if nodeKind != kind {
		if verified != "" {
			return map[string]any{agentplane.AnnAttestationVerified: nil, agentplane.AnnAttestationError: "node " + m.Spec.NodeName + " is not " + kind + " capable"}
		}
		return nil
	}
	if verified == kind {
		return nil
	}
	nonce := ann[agentplane.AnnAttestationNonce]
	if nonce == "" {
		return map[string]any{agentplane.AnnAttestationNonce: attest.NewNonce(), agentplane.AnnAttestationReport: nil}
	}
	report := ann[agentplane.AnnAttestationReport]
	if report == "" {
		return nil
	}
	fail := func(msg string) map[string]any {
		return map[string]any{agentplane.AnnAttestationError: msg, agentplane.AnnAttestationNonce: attest.NewNonce(), agentplane.AnnAttestationReport: nil}
	}
	raw, err := base64.StdEncoding.DecodeString(report)
	if err != nil {
		return fail("report is not base64")
	}
	vctx, cancel := context.WithTimeout(ctx, attestVerifyTimeout)
	defer cancel()
	if err := c.Attest.Verify(vctx, kind, raw, attest.ReportData(m.Metadata.UID, nonce)); err != nil {
		c.Log.Warn("attestation rejected", "namespace", m.Namespace(), "machine", m.Metadata.Name, "kind", kind, "error", err)
		return fail(err.Error())
	}
	c.Log.Info("attestation verified", "namespace", m.Namespace(), "machine", m.Metadata.Name, "kind", kind)
	return map[string]any{
		agentplane.AnnAttestationVerified: kind,
		agentplane.AnnAttestationNonce:    nil,
		agentplane.AnnAttestationReport:   nil,
		agentplane.AnnAttestationError:    nil,
	}
}
