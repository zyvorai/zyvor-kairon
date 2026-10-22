// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package fluxvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/zyvorai/kairon/internal/model"
)

// EgressRule is one entry of FluxVM's QemuEgressPolicy. FluxVM rejects unknown fields, so only
// these four are ever sent.
type EgressRule struct {
	CIDR  string `json:"cidr"`
	Proto string `json:"proto,omitempty"`
	Port  uint16 `json:"port,omitempty"`
	Note  string `json:"note,omitempty"`
}

// EgressPolicy is FluxVM's outbound policy for a VM's QEMU process
// (CreateVmRequest.egress, /v1/vms/{id}/network/egress).
type EgressPolicy struct {
	Mode  string       `json:"mode"`
	Rules []EgressRule `json:"rules"`
}

// WireEgress converts spec.network.egress. nil in, nil out.
func WireEgress(e *model.EgressSpec) *EgressPolicy {
	if e == nil {
		return nil
	}
	p := &EgressPolicy{Mode: e.EffectiveMode(), Rules: []EgressRule{}}
	for _, r := range e.Rules {
		proto := r.Proto
		if proto == "" {
			proto = "any"
		}
		p.Rules = append(p.Rules, EgressRule{CIDR: r.CIDR, Proto: proto, Port: r.Port, Note: r.Note})
	}
	return p
}

// Equivalent reports whether two policies enforce the same thing: same mode and the same set of
// rules, ignoring order, notes and how a bare address or a missing protocol is spelled.
func (p EgressPolicy) Equivalent(o EgressPolicy) bool {
	if p.Mode != o.Mode {
		return false
	}
	norm := func(rs []EgressRule) map[EgressRule]bool {
		out := make(map[EgressRule]bool, len(rs))
		for _, r := range rs {
			r.Note = ""
			if r.Proto == "" {
				r.Proto = "any"
			}
			if pfx, err := parsePrefix(r.CIDR); err == nil {
				r.CIDR = pfx
			}
			out[r] = true
		}
		return out
	}
	a, b := norm(p.Rules), norm(o.Rules)
	if len(a) != len(b) {
		return false
	}
	for r := range a {
		if !b[r] {
			return false
		}
	}
	return true
}

func parsePrefix(s string) (string, error) {
	p, err := model.ParseEgressCIDR(s)
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// GetVMEgress reads the policy FluxVM wants for id (GET /v1/vms/{id}/network/egress).
// source says where it came from: vm, node_default, config or builtin.
func (c *Client) GetVMEgress(ctx context.Context, id string) (policy EgressPolicy, source string, err error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id)+"/network/egress", nil)
	if err != nil {
		return EgressPolicy{}, "", err
	}
	var resp struct {
		Policy EgressPolicy `json:"policy"`
		Source string       `json:"source"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return EgressPolicy{}, "", fmt.Errorf("decode egress policy: %w", err)
	}
	return resp.Policy, resp.Source, nil
}

// SetVMEgress stores the VM's own egress policy; a running VM takes it at once.
func (c *Client) SetVMEgress(ctx context.Context, id string, p EgressPolicy) error {
	_, err := c.do(ctx, http.MethodPut, "/v1/vms/"+url.PathEscape(id)+"/network/egress", p)
	return err
}

// ClearVMEgress drops the VM's own policy so it follows the node default again.
func (c *Client) ClearVMEgress(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(id)+"/network/egress", nil)
	return err
}
