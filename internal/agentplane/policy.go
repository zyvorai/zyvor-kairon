// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package agentplane

import (
	"fmt"
	"net"
	"strings"

	"github.com/zyvorai/kairon/internal/model"
)

// PolicyIntent is the only shape an agent may propose. CompilePolicy
// turns it into a MachineNetworkPolicy with default-deny. The model
// never writes the CRD itself.
type PolicyIntent struct {
	Name          string   `json:"name"`
	Namespace     string   `json:"namespace"`
	Machine       string   `json:"machine,omitempty"`
	Tenant        string   `json:"tenant,omitempty"`
	AllowFQDNs    []string `json:"allowFqdns,omitempty"`
	AllowSNI      []string `json:"allowSNI,omitempty"`
	AllowPorts    []string `json:"allowPorts,omitempty"`
	AllowCIDRs    []string `json:"allowCidrs,omitempty"`
	MaxEgressMbps uint32   `json:"maxEgressMbps,omitempty"`
}

// CompilePolicy emits a strict allowlist. At least one of FQDN, SNI,
// port or CIDR is required. Bare "*" is rejected. A leading "*." is
// the only wildcard, and only on SNI.
func CompilePolicy(in PolicyIntent) (model.MachineNetworkPolicy, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return model.MachineNetworkPolicy{}, fmt.Errorf("policy name is required")
	}
	ns := strings.TrimSpace(in.Namespace)
	if ns == "" {
		ns = "default"
	}
	if in.Tenant != "" {
		if err := ValidTenant(in.Tenant); err != nil {
			return model.MachineNetworkPolicy{}, err
		}
	}
	fqdns, err := cleanNames("allowFqdns", in.AllowFQDNs, false)
	if err != nil {
		return model.MachineNetworkPolicy{}, err
	}
	sni, err := cleanNames("allowSNI", in.AllowSNI, true)
	if err != nil {
		return model.MachineNetworkPolicy{}, err
	}
	ports, err := cleanPorts(in.AllowPorts)
	if err != nil {
		return model.MachineNetworkPolicy{}, err
	}
	cidrs, err := cleanCIDRs(in.AllowCIDRs)
	if err != nil {
		return model.MachineNetworkPolicy{}, err
	}
	if len(fqdns)+len(sni)+len(ports)+len(cidrs) == 0 {
		return model.MachineNetworkPolicy{}, fmt.Errorf("refusing an empty allowlist: name at least one FQDN, SNI, port or CIDR")
	}
	var mbps *uint32
	if in.MaxEgressMbps > 0 {
		v := in.MaxEgressMbps
		mbps = &v
	}
	pol := model.MachineNetworkPolicy{
		Metadata: model.ObjectMeta{Name: name, Namespace: ns},
		Spec: model.MachineNetworkPolicySpec{
			MachineName: strings.TrimSpace(in.Machine),
			Policy: model.VmNetworkPolicy{
				DefaultAllow:  false,
				AllowFqdns:    fqdns,
				AllowDNS:      fqdns,
				AllowSNI:      sni,
				AllowPorts:    ports,
				AllowCidrs:    cidrs,
				MaxEgressMbps: mbps,
			},
		},
	}
	if in.Tenant != "" {
		if pol.Metadata.Labels == nil {
			pol.Metadata.Labels = map[string]string{}
		}
		pol.Metadata.Labels["kairon.zyvor.dev/tenant"] = in.Tenant
	}
	return pol, nil
}

func cleanNames(field string, in []string, allowWildcard bool) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		n := strings.ToLower(strings.TrimSpace(raw))
		if n == "" {
			continue
		}
		if n == "*" || strings.Contains(n, " ") {
			return nil, fmt.Errorf("%s entry %q is not a strict name", field, raw)
		}
		if strings.HasPrefix(n, "*.") {
			if !allowWildcard {
				return nil, fmt.Errorf("%s entry %q: wildcard only allowed on SNI", field, raw)
			}
			rest := strings.TrimPrefix(n, "*.")
			if rest == "" || strings.Contains(rest, "*") || strings.Contains(rest, "..") {
				return nil, fmt.Errorf("%s entry %q is not a suffix wildcard", field, raw)
			}
		} else if strings.Contains(n, "*") || strings.Contains(n, "..") {
			return nil, fmt.Errorf("%s entry %q is not a strict name", field, raw)
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out, nil
}

func cleanPorts(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if p == "*" || strings.ContainsAny(p, " /") {
			return nil, fmt.Errorf("allowPorts entry %q is not a port or port range", raw)
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out, nil
}

func cleanCIDRs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, raw := range in {
		c := strings.TrimSpace(raw)
		if c == "" {
			continue
		}
		if c == "0.0.0.0/0" || c == "::/0" {
			return nil, fmt.Errorf("allowCidrs entry %q is not a strict allowlist", raw)
		}
		if _, _, err := net.ParseCIDR(c); err != nil {
			return nil, fmt.Errorf("allowCidrs entry %q: %w", raw, err)
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}
