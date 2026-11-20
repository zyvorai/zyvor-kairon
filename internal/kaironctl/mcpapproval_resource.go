// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/fleet"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
)

func mcpResourceApproveHook(nsDefault string) func(context.Context, string, json.RawMessage) (string, error) {
	return func(ctx context.Context, tool string, args json.RawMessage) (string, error) {
		target, needed, err := mcpApprovalTarget(tool, args, nsDefault)
		if err != nil || !needed {
			return "", err
		}
		client, err := newKubeClient()
		if err != nil {
			return "", err
		}
		principal, err := client.AuthenticatedUsername(ctx)
		if err != nil {
			return "", err
		}
		hash, err := fleet.ArgumentsHash(tool, args, principal)
		if err != nil {
			return "", err
		}
		resource := approvalKinds[target.Kind]
		var uid string
		var generation int64
		if resource == "machines" {
			m, err := client.GetMachine(ctx, target.Namespace, target.Name)
			if err != nil {
				return "", err
			}
			uid = m.Metadata.UID
			generation = m.Metadata.Generation
		} else {
			b, err := client.GetMachineBackup(ctx, target.Namespace, target.Name)
			if err != nil {
				return "", err
			}
			uid = b.Metadata.UID
			generation = b.Metadata.Generation
		}
		expected := fleet.ApprovalSpec{Action: tool, ArgumentsHash: hash, Principal: principal, TargetResource: resource, TargetName: target.Name, TargetUID: uid, TargetGeneration: generation}
		approver, err := fleet.ConsumeApproval(ctx, client, target.Namespace, expected, time.Now().UTC())
		if err == nil {
			return "approved:" + approver, nil
		}
		if !kube.IsNotFound(err) {
			return "", err
		}
		request, _ := json.MarshalIndent(expected, "", "  ")
		return "", &mcp.ApprovalError{Outcome: "approval-required", Message: fmt.Sprintf("Separate Kubernetes approver required. Save this exact request and run kaironctl fleet approve-action REQUEST.json -n %s using the approver's own credential, then retry:\n%s", target.Namespace, string(request))}
	}
}
