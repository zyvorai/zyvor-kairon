// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateEgress(t *testing.T) {
	cases := []struct {
		name    string
		egress  *EgressSpec
		wantErr string
	}{
		{"unset", nil, ""},
		{"empty", &EgressSpec{}, ""},
		{"platform rule", &EgressSpec{Mode: "allowlist", Rules: []EgressRule{{CIDR: "10.0.0.0/8", Proto: "tcp", Port: 5151}}}, ""},
		{"bare address", &EgressSpec{Rules: []EgressRule{{CIDR: "192.168.1.5"}}}, ""},
		{"bad mode", &EgressSpec{Mode: "loose"}, "invalid"},
		{"ipv6", &EgressSpec{Rules: []EgressRule{{CIDR: "fe80::/10"}}}, "IPv6"},
		{"garbage cidr", &EgressSpec{Rules: []EgressRule{{CIDR: "nope"}}}, "cidr"},
		{"prefix too long", &EgressSpec{Rules: []EgressRule{{CIDR: "10.0.0.0/33"}}}, "cidr"},
		{"port without proto", &EgressSpec{Rules: []EgressRule{{CIDR: "10.0.0.0/8", Port: 443}}}, "needs proto"},
		{"bad proto", &EgressSpec{Rules: []EgressRule{{CIDR: "10.0.0.0/8", Proto: "icmp"}}}, "proto"},
		{"five ports on one network", func() *EgressSpec {
			e := &EgressSpec{}
			for p := uint16(8001); p <= 8005; p++ {
				e.Rules = append(e.Rules, EgressRule{CIDR: "10.0.0.0/8", Proto: "tcp", Port: p})
			}
			return e
		}(), "at most 4"},
		{"folding pushes a narrow network over the limit", &EgressSpec{Rules: []EgressRule{
			{CIDR: "10.0.0.0/8", Proto: "tcp", Port: 1}, {CIDR: "10.0.0.0/8", Proto: "tcp", Port: 2},
			{CIDR: "10.0.0.0/8", Proto: "tcp", Port: 3}, {CIDR: "10.1.0.0/16", Proto: "tcp", Port: 4},
			{CIDR: "10.1.0.0/16", Proto: "tcp", Port: 5},
		}}, "at most 4"},
		{"an any-port rule on the covering network is fine", &EgressSpec{Rules: []EgressRule{
			{CIDR: "10.0.0.0/8"}, {CIDR: "10.1.0.0/16", Proto: "tcp", Port: 1}, {CIDR: "10.1.0.0/16", Proto: "tcp", Port: 2},
			{CIDR: "10.1.0.0/16", Proto: "tcp", Port: 3}, {CIDR: "10.1.0.0/16", Proto: "tcp", Port: 4}, {CIDR: "10.1.0.0/16", Proto: "tcp", Port: 5},
		}}, ""},
		{"too many rules", func() *EgressSpec {
			e := &EgressSpec{}
			for i := 0; i <= MaxEgressRules; i++ {
				e.Rules = append(e.Rules, EgressRule{CIDR: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)})
			}
			return e
		}(), "limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateEgress(NetworkSpec{Egress: c.egress})
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestEgressEffectiveMode(t *testing.T) {
	if got := (EgressSpec{}).EffectiveMode(); got != EgressLoopback {
		t.Fatalf("empty = %q", got)
	}
	if got := (EgressSpec{Rules: []EgressRule{{CIDR: "10.0.0.0/8"}}}).EffectiveMode(); got != EgressAllowlist {
		t.Fatalf("rules only = %q", got)
	}
	if got := (EgressSpec{Mode: EgressAudit}).EffectiveMode(); got != EgressAudit {
		t.Fatalf("audit = %q", got)
	}
}
