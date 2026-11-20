// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"fmt"
	"time"

	"github.com/zyvorai/kairon/internal/model"
)

const (
	ActionBind    = "bind"
	ActionHold    = "hold"
	ActionWait    = "wait"
	ActionExpire  = "expire"
	ActionRelease = "release"

	AnnSnapshotOnRelease = "kairon.zyvor.dev/snapshot-on-release"
)

// WarmMachine is a pool member a claim can bind. Only Running + warm
// members are eligible.
type WarmMachine struct {
	Name       string `json:"name"`
	Tenant     string `json:"tenant,omitempty"`
	Phase      string `json:"phase,omitempty"`
	PoolState  string `json:"poolState,omitempty"`
	Hypervisor string `json:"hypervisor,omitempty"`
}

// ClaimDecision is what the controller should do on this tick. It does
// not write the apiserver.
type ClaimDecision struct {
	Action     string `json:"action"`
	Machine    string `json:"machine,omitempty"`
	PolicyName string `json:"policyName,omitempty"`
	Snapshot   bool   `json:"snapshot,omitempty"`
	Reclaim    string `json:"reclaim,omitempty"`
	Message    string `json:"message"`
}

// StepClaim advances one MachineClaim. Expired bound claims release.
// Pending claims bind the first eligible warm Machine. Egress, when
// set, is compiled before a bind is allowed.
func StepClaim(now time.Time, claim model.MachineClaim, warm []WarmMachine) (ClaimDecision, error) {
	if claim.Metadata.Name == "" || claim.Spec.PoolName == "" {
		return ClaimDecision{}, fmt.Errorf("claim name and poolName are required")
	}
	if claim.Spec.Egress != nil {
		if _, err := CompilePolicy(PolicyIntent{
			Name:       model.ClaimEgressPolicyName(claim.Metadata.Name),
			Namespace:  claim.Metadata.Namespace,
			AllowFQDNs: claim.Spec.Egress.AllowFqdns,
			AllowSNI:   claim.Spec.Egress.AllowSNI,
			AllowPorts: claim.Spec.Egress.AllowPorts,
			AllowCIDRs: claim.Spec.Egress.AllowCidrs,
		}); err != nil {
			return ClaimDecision{}, fmt.Errorf("egress: %w", err)
		}
	}
	reclaim := claim.Spec.ReclaimPolicy
	if reclaim == "" {
		reclaim = model.ReclaimDelete
	}
	if reclaim != model.ReclaimDelete && reclaim != model.ReclaimRetain {
		return ClaimDecision{}, fmt.Errorf("reclaimPolicy %q is not Delete or Retain", reclaim)
	}
	snap := false
	if claim.Metadata.Annotations != nil {
		snap = truthy(claim.Metadata.Annotations[AnnSnapshotOnRelease])
	}
	if claim.Status.Phase == model.ClaimBound && claim.Status.BoundAt != nil && claim.Spec.TTLSeconds > 0 {
		deadline := claim.Status.BoundAt.Add(time.Duration(claim.Spec.TTLSeconds) * time.Second)
		if !now.Before(deadline) {
			return ClaimDecision{
				Action: ActionExpire, Machine: claim.Status.MachineName, Snapshot: snap, Reclaim: reclaim,
				PolicyName: policyName(claim),
				Message:    "claim TTL elapsed",
			}, nil
		}
		return ClaimDecision{Action: ActionHold, Machine: claim.Status.MachineName, Message: "claim still inside TTL"}, nil
	}
	if claim.Status.Phase == model.ClaimBound {
		return ClaimDecision{Action: ActionHold, Machine: claim.Status.MachineName, Message: "claim bound"}, nil
	}
	wantHV := ""
	if claim.Metadata.Annotations != nil {
		wantHV = claim.Metadata.Annotations[AnnHypervisor]
	}
	for _, m := range warm {
		if m.Phase != "" && m.Phase != "Running" {
			continue
		}
		if m.PoolState != "" && m.PoolState != model.PoolStateWarm {
			continue
		}
		if claim.Spec.Labels["kairon.zyvor.dev/tenant"] != "" && m.Tenant != "" && m.Tenant != claim.Spec.Labels["kairon.zyvor.dev/tenant"] {
			continue
		}
		if wantHV != "" && m.Hypervisor != "" && m.Hypervisor != wantHV {
			continue
		}
		return ClaimDecision{
			Action: ActionBind, Machine: m.Name, Snapshot: false, Reclaim: reclaim,
			PolicyName: policyName(claim),
			Message:    "bind warm machine",
		}, nil
	}
	return ClaimDecision{Action: ActionWait, Message: "no eligible warm machine"}, nil
}

func policyName(claim model.MachineClaim) string {
	if claim.Spec.Egress == nil {
		return ""
	}
	return model.ClaimEgressPolicyName(claim.Metadata.Name)
}
