// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package kaironctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/agentplane"
	"github.com/zyvorai/kairon/internal/kube"
	"github.com/zyvorai/kairon/internal/mcp"
	"github.com/zyvorai/kairon/internal/model"
)

const labelTenant = "kairon.zyvor.dev/tenant"

func claimTenant(c model.MachineClaim) string {
	if t := c.Spec.Labels[labelTenant]; t != "" {
		return t
	}
	return c.Metadata.Labels[labelTenant]
}

// mcpTenantVisible reports whether the MCP caller may see an object of
// tenant. An unscoped caller sees everything.
func mcpTenantVisible(tenant string) bool {
	caller := strings.TrimSpace(os.Getenv("KAIRON_MCP_TENANT"))
	return caller == "" || caller == tenant
}

type claimSummary struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Pool         string `json:"pool"`
	Tenant       string `json:"tenant,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Machine      string `json:"machine,omitempty"`
	TTLSeconds   int64  `json:"ttlSeconds,omitempty"`
	EgressPolicy string `json:"egressPolicy,omitempty"`
	Message      string `json:"message,omitempty"`
}

func summarizeClaim(c model.MachineClaim) claimSummary {
	return claimSummary{
		Namespace: c.Namespace(), Name: c.Metadata.Name, Pool: c.Spec.PoolName, Tenant: claimTenant(c),
		Phase: c.Status.Phase, Machine: c.Status.MachineName, TTLSeconds: c.Spec.TTLSeconds,
		EgressPolicy: c.Status.EgressPolicy, Message: c.Status.Message,
	}
}

type claimArgs struct {
	Namespace         string                  `json:"namespace"`
	Name              string                  `json:"name"`
	Pool              string                  `json:"pool"`
	Tenant            string                  `json:"tenant"`
	TTLSeconds        int                     `json:"ttlSeconds"`
	Hypervisor        string                  `json:"hypervisor"`
	ReclaimPolicy     string                  `json:"reclaimPolicy"`
	SnapshotOnRelease bool                    `json:"snapshotOnRelease"`
	Egress            agentplane.PolicyIntent `json:"egress"`
}

// buildClaim validates args with ValidateClaim and returns the
// MachineClaim the controller will bind.
func buildClaim(a claimArgs, nsDefault string) (model.MachineClaim, error) {
	ns := a.Namespace
	if ns == "" {
		ns = nsOrDefault(nsDefault)
	}
	if err := callerTenant(a.Tenant); err != nil {
		return model.MachineClaim{}, err
	}
	a.Egress.Name = model.ClaimEgressPolicyName(a.Name)
	a.Egress.Namespace = ns
	a.Egress.Tenant = a.Tenant
	if err := agentplane.ValidateClaim(agentplane.ClaimRequest{
		Pool: a.Pool, Name: a.Name, Tenant: a.Tenant, TTLSec: a.TTLSeconds, Hypervisor: a.Hypervisor, Egress: a.Egress,
	}); err != nil {
		return model.MachineClaim{}, err
	}
	switch a.ReclaimPolicy {
	case "", model.ReclaimRetain, "Delete":
	default:
		return model.MachineClaim{}, fmt.Errorf("reclaimPolicy must be Delete or Retain")
	}
	ann := map[string]string{}
	if a.Hypervisor != "" {
		ann[agentplane.AnnHypervisor] = a.Hypervisor
	}
	if a.SnapshotOnRelease {
		ann[agentplane.AnnSnapshotOnRelease] = "true"
	}
	return model.MachineClaim{
		TypeMeta: model.TypeMeta{APIVersion: "kairon.zyvor.dev/v1", Kind: "MachineClaim"},
		Metadata: model.ObjectMeta{Name: a.Name, Namespace: ns, Labels: map[string]string{labelTenant: a.Tenant}, Annotations: ann},
		Spec: model.MachineClaimSpec{
			PoolName:      a.Pool,
			Labels:        map[string]string{labelTenant: a.Tenant},
			ReclaimPolicy: a.ReclaimPolicy,
			TTLSeconds:    int64(a.TTLSeconds),
			Egress: &model.ClaimEgress{
				AllowFqdns: a.Egress.AllowFQDNs,
				AllowSNI:   a.Egress.AllowSNI,
				AllowPorts: a.Egress.AllowPorts,
				AllowCidrs: a.Egress.AllowCIDRs,
			},
		},
	}, nil
}

// agentPlaneKubeTools read and change MachineClaims, MachinePools and
// MachineNetworkPolicies. Writes accept only validated or compiled
// objects, and every write is audited by the server.
func agentPlaneKubeTools(nsDefault string, withKube func(context.Context, time.Duration, func(context.Context, *kube.Client) (string, error)) (string, error)) []mcp.Tool {
	nsProp := mcp.String("Kubernetes namespace; defaults to " + nsOrDefault(nsDefault))
	ns := func(v string) string {
		if v != "" {
			return v
		}
		return nsOrDefault(nsDefault)
	}
	getClaim := func(ctx context.Context, kc *kube.Client, namespace, name string) (model.MachineClaim, error) {
		c, err := kc.GetMachineClaim(ctx, ns(namespace), name)
		if err != nil {
			return model.MachineClaim{}, err
		}
		if !mcpTenantVisible(claimTenant(c)) {
			return model.MachineClaim{}, fmt.Errorf("machineclaim %s/%s not found", ns(namespace), name)
		}
		return c, nil
	}
	claimProps := map[string]any{
		"namespace":         nsProp,
		"name":              mcp.String("claim name"),
		"pool":              mcp.String("MachinePool in the same namespace"),
		"tenant":            mcp.String("tenant (DNS label)"),
		"ttlSeconds":        map[string]any{"type": "integer", "description": "30 to 86400"},
		"hypervisor":        mcp.String("firecracker, cloud-hypervisor, qemu or fluxvm"),
		"reclaimPolicy":     mcp.String("Delete (default) or Retain"),
		"snapshotOnRelease": map[string]any{"type": "boolean"},
		"egress":            map[string]any{"type": "object", "description": "PolicyIntent: allowFqdns, allowSNI, allowPorts, allowCidrs"},
	}
	return []mcp.Tool{
		{
			Name:        "list_claims",
			Description: "List MachineClaims with pool, tenant, phase, bound Machine and TTL. Scoped to KAIRON_MCP_TENANT when set.",
			Schema:      mcp.Object(map[string]any{"namespace": mcp.String("namespace; omit for all")}),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					var items []model.MachineClaim
					var err error
					if a.Namespace != "" {
						items, err = kc.ListMachineClaimsNamespace(ctx, a.Namespace)
					} else {
						items, err = kc.ListMachineClaims(ctx)
					}
					if err != nil {
						return "", err
					}
					out := make([]claimSummary, 0, len(items))
					for _, c := range items {
						if mcpTenantVisible(claimTenant(c)) {
							out = append(out, summarizeClaim(c))
						}
					}
					return mcp.JSON(map[string]any{"claims": out})
				})
			},
		},
		{
			Name:        "describe_claim",
			Description: "Get one MachineClaim's spec and status plus the agent-plane step decision for it right now.",
			Schema:      mcp.Object(map[string]any{"namespace": nsProp, "name": mcp.String("claim name")}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					c, err := getClaim(ctx, kc, a.Namespace, a.Name)
					if err != nil {
						return "", err
					}
					decision, derr := stepLive(ctx, kc, c)
					return mcp.JSON(map[string]any{"claim": c, "decision": decision, "decisionError": errString(derr)})
				})
			},
		},
		{
			Name:        "diagnose",
			Description: "Rank likely causes for a stuck or failed Machine or MachineMigration from its phase, conditions and Warning events, with proposed next steps. Adds a model summary when KAIRON_LLM_URL is configured. Read-only.",
			Schema: mcp.Object(map[string]any{
				"namespace": nsProp,
				"ref":       mcp.String("machine/NAME or migration/NAME (a bare name is a Machine)"),
			}, "ref"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
					Ref       string `json:"ref"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, 2*mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					d, err := diagnoseRef(ctx, kc, ns(a.Namespace), a.Ref, true)
					if err != nil {
						return "", err
					}
					return mcp.JSON(d)
				})
			},
		},
		{
			Name:        "create_sealed_claim",
			Description: "Create a sealed MachineClaim. Validated first: tenant, TTL 30-86400, hypervisor, and a non-empty strict egress allowlist. Requires --allow-write; audited.",
			Write:       true,
			Schema:      mcp.Object(claimProps, "name", "pool", "tenant", "ttlSeconds", "egress"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a claimArgs
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				claim, err := buildClaim(a, nsDefault)
				if err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					created, err := kc.CreateMachineClaim(ctx, claim.Namespace(), claim)
					if err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"created": summarizeClaim(created)})
				})
			},
		},
		{
			Name:        "apply_network_policy",
			Description: "Compile a PolicyIntent with the strict compiler and create or update the resulting MachineNetworkPolicy. Raw policies are not accepted. Requires --allow-write; audited.",
			Write:       true,
			Schema:      mcp.Object(map[string]any{"intent": map[string]any{"type": "object", "description": "PolicyIntent JSON"}}, "intent"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Intent agentplane.PolicyIntent `json:"intent"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				if err := callerTenant(a.Intent.Tenant); err != nil {
					return "", err
				}
				pol, err := agentplane.CompilePolicy(a.Intent)
				if err != nil {
					return "", err
				}
				pol.TypeMeta = model.TypeMeta{APIVersion: "kairon.zyvor.dev/v1", Kind: "MachineNetworkPolicy"}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					existing, err := kc.GetMachineNetworkPolicy(ctx, pol.Namespace(), pol.Metadata.Name)
					switch {
					case kube.IsNotFound(err):
						if _, err := kc.CreateMachineNetworkPolicy(ctx, pol.Namespace(), pol); err != nil {
							return "", err
						}
						return mcp.JSON(map[string]any{"created": pol.Namespace() + "/" + pol.Metadata.Name, "policy": pol.Spec})
					case err != nil:
						return "", err
					}
					if t := existing.Metadata.Labels[labelTenant]; t != "" && !mcpTenantVisible(t) {
						return "", fmt.Errorf("machinenetworkpolicy %s/%s belongs to another tenant", pol.Namespace(), pol.Metadata.Name)
					}
					if err := kc.PatchMachineNetworkPolicy(ctx, pol.Namespace(), pol.Metadata.Name, map[string]any{"spec": pol.Spec}); err != nil {
						return "", err
					}
					return mcp.JSON(map[string]any{"updated": pol.Namespace() + "/" + pol.Metadata.Name, "policy": pol.Spec})
				})
			},
		},
		{
			Name:        "apply_claim_step",
			Description: "Run the claim step against live pool members and apply it. Only expire is applied here (the claim is deleted and released); bind, hold and wait are applied by kairon-controller. Requires --allow-write; audited.",
			Write:       true,
			Schema:      mcp.Object(map[string]any{"namespace": nsProp, "name": mcp.String("claim name")}, "name"),
			Call: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var a struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				}
				if err := decodeArgs(raw, &a); err != nil {
					return "", err
				}
				return withKube(ctx, mcpCallTimeout, func(ctx context.Context, kc *kube.Client) (string, error) {
					c, err := getClaim(ctx, kc, a.Namespace, a.Name)
					if err != nil {
						return "", err
					}
					decision, err := stepLive(ctx, kc, c)
					if err != nil {
						return "", err
					}
					applied := false
					if decision.Action == agentplane.ActionExpire {
						if err := kc.DeleteMachineClaim(ctx, c.Namespace(), c.Metadata.Name); err != nil && !kube.IsNotFound(err) {
							return "", err
						}
						applied = true
					}
					return mcp.JSON(map[string]any{"decision": decision, "applied": applied})
				})
			},
		},
	}
}

// stepLive runs StepClaim with the claim's pool members as the warm set.
func stepLive(ctx context.Context, kc *kube.Client, c model.MachineClaim) (agentplane.ClaimDecision, error) {
	machines, err := kc.ListMachinesNamespace(ctx, c.Namespace())
	if err != nil {
		return agentplane.ClaimDecision{}, err
	}
	var warm []agentplane.WarmMachine
	for _, m := range machines {
		if m.Metadata.Labels[model.LabelMachinePool] != c.Spec.PoolName {
			continue
		}
		if !agentplane.EligibleWarm(c, m) {
			continue
		}
		warm = append(warm, agentplane.WarmMachine{
			Name: m.Metadata.Name, Tenant: m.Spec.Tenant, Phase: m.Status.Phase,
			PoolState: m.Metadata.Labels[model.LabelPoolState], Hypervisor: m.Metadata.Annotations[agentplane.AnnHypervisor],
		})
	}
	return agentplane.StepClaim(time.Now().UTC(), c, warm)
}
