// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/model"
)

func ArgumentsHash(action string, args json.RawMessage, principal string) (string, error) {
	var parsed any
	if err := json.Unmarshal(args, &parsed); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(parsed)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(action + "\n" + string(canonical) + "\n" + principal))
	return hex.EncodeToString(h[:]), nil
}
func ApprovalName(spec ApprovalSpec) string {
	hash := sha256.Sum256([]byte(spec.ArgumentsHash + "/" + spec.TargetUID + fmt.Sprintf("/%d", spec.TargetGeneration)))
	return "approval-" + hex.EncodeToString(hash[:])[:32]
}

// ConsumeApproval atomically spends approval before the action runs. Approval
// specs are immutable and admission binds Approver to Kubernetes userInfo.
func ConsumeApproval(ctx context.Context, c *kube.Client, ns string, expected ApprovalSpec, now time.Time) (string, error) {
	name := ApprovalName(expected)
	o, err := c.GetFleet(ctx, ns, "machineactionapprovals", name)
	if err != nil {
		return "", err
	}
	if err := Validate(o); err != nil {
		return "", err
	}
	actual, err := decode[ApprovalSpec](o.Spec)
	if err != nil {
		return "", err
	}
	if actual.Action != expected.Action || actual.ArgumentsHash != expected.ArgumentsHash || actual.Principal != expected.Principal || actual.TargetResource != expected.TargetResource || actual.TargetName != expected.TargetName || actual.TargetUID != expected.TargetUID || actual.TargetGeneration != expected.TargetGeneration {
		return "", fmt.Errorf("approval no longer matches the action or target")
	}
	if actual.Approver == actual.Principal {
		return "", fmt.Errorf("an agent cannot approve its own action")
	}
	if !now.Before(actual.ExpiresAt) || actual.ExpiresAt.After(now.Add(10*time.Minute)) {
		return "", fmt.Errorf("approval expired or exceeds maximum TTL")
	}
	if o.Status.Phase != "" {
		return "", fmt.Errorf("approval has already been consumed")
	}
	status := model.FleetStatus{Phase: "Consumed", Message: "consumed by " + expected.Principal, LastActionTime: &now, ObservedGeneration: o.Metadata.Generation}
	if err := c.PatchFleetStatus(ctx, "machineactionapprovals", o, status); err != nil {
		return "", err
	}
	return actual.Approver, nil
}
