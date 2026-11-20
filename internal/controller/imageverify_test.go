// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/admission"
	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/model"
)

type fakeVerifier struct {
	err  error
	refs []string
}

func (f *fakeVerifier) Verify(_ context.Context, ref, digest string) error {
	f.refs = append(f.refs, ref+"@"+digest)
	return f.err
}

func agentPoolMachine() model.Machine {
	digest := "sha256:" + strings.Repeat("a", 64)
	return model.Machine{
		Metadata: model.ObjectMeta{Namespace: "prod", Name: "agent-1", Annotations: map[string]string{
			agentplane.AnnAgentPool: "true",
			agentplane.AnnImageSign: "cosign:sha256:" + strings.Repeat("b", 64),
			agentplane.AnnEgress:    "registry.internal",
		}},
		Spec: model.MachineSpec{
			Tenant: "acme",
			Image:  model.ImageSpec{Digest: digest, Source: &model.ImageSource{OCI: "registry.internal/agents/runner:v1"}},
		},
	}
}

func TestWebhookAgentPoolImageSignature(t *testing.T) {
	ctl := newWebhookTestController(t, "prod", nil, nil, nil, nil)
	admit := func(m model.Machine) admission.Decision {
		r, req := admissionReq(t, "machines", "prod", admission.OperationCreate, m)
		return ctl.validateMachine(r, req)
	}

	if d := admit(agentPoolMachine()); !d.Allowed {
		t.Fatalf("without a verifier the shape check alone should allow: %s", d.Reason)
	}

	noEgress := agentPoolMachine()
	delete(noEgress.Metadata.Annotations, agentplane.AnnEgress)
	if d := admit(noEgress); d.Allowed || !strings.Contains(d.Reason, agentplane.AnnEgress) {
		t.Fatalf("agent pool without egress allowlist must be denied: %+v", d)
	}

	v := &fakeVerifier{}
	ctl.Cosign = v
	if d := admit(agentPoolMachine()); !d.Allowed || len(v.refs) != 1 || !strings.HasPrefix(v.refs[0], "registry.internal/agents/runner:v1@sha256:aaa") {
		t.Fatalf("signed image: %+v refs=%v", d, v.refs)
	}

	v.err = errors.New("signature does not verify")
	if d := admit(agentPoolMachine()); d.Allowed || !strings.Contains(d.Reason, "signature does not verify") {
		t.Fatalf("bad signature must be denied: %+v", d)
	}

	pathOnly := agentPoolMachine()
	pathOnly.Spec.Image = model.ImageSpec{Path: "/var/lib/images/x.raw", Digest: pathOnly.Spec.Image.Digest}
	if d := admit(pathOnly); d.Allowed || !strings.Contains(d.Reason, "spec.image.source.oci") {
		t.Fatalf("non-OCI agent pool image must be denied when verifying: %+v", d)
	}

	plain := model.Machine{Metadata: model.ObjectMeta{Namespace: "prod", Name: "plain"}, Spec: model.MachineSpec{Image: model.ImageSpec{Path: "/var/lib/images/x.raw"}}}
	calls := len(v.refs)
	if d := admit(plain); !d.Allowed || len(v.refs) != calls {
		t.Fatalf("non agent-pool Machine must not be verified: %+v", d)
	}
}
