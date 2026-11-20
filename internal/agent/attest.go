// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agent

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/attest"
	"github.com/zyvorai/kairon/internal/model"
)

const (
	capabilityEvery    = 10 * time.Minute
	attestRetryEvery   = time.Minute
	attestGuestTimeout = 30
)

type attestState struct {
	mu      sync.Mutex
	capAt   time.Time
	capSeen bool
	tried   map[string]time.Time
}

// DetectConfidential reports which confidential kind the host kernel can
// run: sev-snp when kvm_amd has sev_snp on, tdx when kvm_intel has tdx
// on, else "". sysRoot is normally /sys.
func DetectConfidential(sysRoot string) string {
	on := func(rel string) bool {
		b, err := os.ReadFile(filepath.Join(sysRoot, rel))
		if err != nil {
			return false
		}
		switch strings.TrimSpace(string(b)) {
		case "Y", "y", "1":
			return true
		}
		return false
	}
	switch {
	case on("module/kvm_amd/parameters/sev_snp"):
		return attest.KindSNP
	case on("module/kvm_intel/parameters/tdx"):
		return attest.KindTDX
	}
	return ""
}

// publishConfidentialCapability keeps this node's confidential-capable
// label in step with the kernel. A kernel that loses the capability
// removes the label, so the controller stops sealing Machines here.
func (a *Agent) publishConfidentialCapability(ctx context.Context) {
	if a.Kube == nil || a.NodeName == "" {
		return
	}
	s := &a.attest
	s.mu.Lock()
	due := !s.capSeen || time.Since(s.capAt) >= capabilityEvery
	if due {
		s.capAt, s.capSeen = time.Now(), true
	}
	s.mu.Unlock()
	if !due {
		return
	}
	root := a.SysRoot
	if root == "" {
		root = "/sys"
	}
	kind := DetectConfidential(root)
	node, err := a.Kube.GetNode(ctx, a.NodeName)
	if err != nil {
		a.log().Debug("node read for confidential label failed", "error", err)
		return
	}
	have, set := node.Metadata.Labels[agentplane.LabelConfidentialCapable]
	if have == kind && (set || kind == "") {
		return
	}
	var value any = kind
	if kind == "" {
		value = nil
	}
	patch := map[string]any{"metadata": map[string]any{"labels": map[string]any{agentplane.LabelConfidentialCapable: value}}}
	if err := a.Kube.PatchNode(ctx, a.NodeName, patch); err != nil {
		a.log().Warn("confidential capability label patch failed", "node", a.NodeName, "error", err)
		return
	}
	a.log().Info("confidential capability label updated", "node", a.NodeName, "kind", kind)
}

// serveAttestation fetches a report from the guest for the controller's
// current nonce and puts it on the Machine. The report is not trusted
// here; the controller verifies it.
func (a *Agent) serveAttestation(ctx context.Context, m model.Machine) {
	ann := m.Metadata.Annotations
	kind := strings.ToLower(strings.TrimSpace(ann[agentplane.AnnConfidential]))
	nonce := ann[agentplane.AnnAttestationNonce]
	if a.Kube == nil || a.Flux == nil || (kind != attest.KindSNP && kind != attest.KindTDX) || nonce == "" ||
		ann[agentplane.AnnAttestationReport] != "" || ann[agentplane.AnnAttestationVerified] == kind ||
		m.Status.RuntimeID == "" || m.Status.Phase != "Running" || !m.Spec.GuestAgent.Console {
		return
	}
	key := m.Namespace() + "/" + m.Metadata.Name + "|" + nonce
	s := &a.attest
	s.mu.Lock()
	if s.tried == nil {
		s.tried = map[string]time.Time{}
	}
	if t, ok := s.tried[key]; ok && time.Since(t) < attestRetryEvery {
		s.mu.Unlock()
		return
	}
	for k, t := range s.tried {
		if time.Since(t) >= attestRetryEvery {
			delete(s.tried, k)
		}
	}
	s.tried[key] = time.Now()
	s.mu.Unlock()

	data := attest.ReportData(m.Metadata.UID, nonce)
	timeout := uint64(attestGuestTimeout)
	res, err := a.Flux.AgentExec(ctx, m.Status.RuntimeID, attestGuestCommand(a.AttestCommand, kind, hex.EncodeToString(data[:])), &timeout)
	if err != nil || res.ExitCode != 0 {
		msg := ""
		if res != nil {
			msg = strings.TrimSpace(res.Stderr)
		}
		a.log().Debug("guest attestation report failed", "machine", m.Metadata.Name, "error", err, "stderr", msg)
		return
	}
	report := strings.Join(strings.Fields(res.Stdout), "")
	if _, err := base64.StdEncoding.DecodeString(report); err != nil || report == "" {
		a.log().Debug("guest attestation report is not base64", "machine", m.Metadata.Name)
		return
	}
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{agentplane.AnnAttestationReport: report}}}
	if err := a.Kube.PatchMachine(ctx, m.Namespace(), m.Metadata.Name, patch); err != nil {
		a.log().Warn("attestation report patch failed", "machine", m.Metadata.Name, "error", err)
	}
}

// attestGuestCommand fills {kind} and {data} (hex REPORT_DATA). The
// default uses the attest tools from go-sev-guest and go-tdx-guest.
func attestGuestCommand(tmpl, kind, data string) string {
	if tmpl == "" {
		tmpl = "attest -inform hex -in {data} -outform bin | base64 -w0"
		if kind == attest.KindSNP {
			tmpl = "attest -extended -inform hex -in {data} -outform bin | base64 -w0"
		}
	}
	return strings.NewReplacer("{kind}", kind, "{data}", data).Replace(tmpl)
}
