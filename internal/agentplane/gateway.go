// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// PortForward is the slice of spec.network.forwards an operator wants
// in front of a Gateway, not as a host port.
type PortForward struct {
	Protocol  string `json:"protocol"`
	HostPort  int    `json:"hostPort"`
	GuestPort int    `json:"guestPort"`
}

type GatewayBinding struct {
	Name  string        `json:"name"`
	Ports []PortForward `json:"ports"`
}

func BindGateway(name string, forwards []PortForward) (GatewayBinding, error) {
	if name == "" {
		return GatewayBinding{}, fmt.Errorf("gateway name is required")
	}
	if len(forwards) == 0 {
		return GatewayBinding{}, fmt.Errorf("at least one forward is required")
	}
	out := GatewayBinding{Name: name}
	for _, f := range forwards {
		proto := f.Protocol
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" && proto != "udp" {
			return GatewayBinding{}, fmt.Errorf("protocol %q is not tcp or udp", f.Protocol)
		}
		if f.GuestPort < 1 || f.GuestPort > 65535 || f.HostPort < 1 || f.HostPort > 65535 {
			return GatewayBinding{}, fmt.Errorf("ports must be 1-65535")
		}
		out.Ports = append(out.Ports, PortForward{Protocol: proto, HostPort: f.HostPort, GuestPort: f.GuestPort})
	}
	return out, nil
}
