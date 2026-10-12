// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package scheduler

import (
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// wantsVZ reports whether m runs on Apple Virtualization.framework.
func wantsVZ(m model.Machine) bool {
	return m.Spec.Runtime.Backend == "vz" || m.Spec.Apple != nil
}

func isMac(n model.Node) bool {
	return n.Metadata.Labels[model.LabelBackendVZ] == "true"
}

// appleEligible keeps vz Machines on Macs and other backends off them,
// then checks spec.apple against the Mac's labels and MacNetwork
// membership.
func appleEligible(m model.Machine, n model.Node, topo TopologyInput) (bool, string) {
	vz, mac := wantsVZ(m), isMac(n)
	switch {
	case vz && !mac:
		return false, "the vz backend needs a Mac (" + model.LabelBackendVZ + "=true)"
	case !vz && mac:
		if b := m.Spec.Runtime.Backend; b != "" && b != "auto" {
			return false, fmt.Sprintf("a Mac runs only the vz backend, Machine asks for %q", b)
		}
		return true, ""
	case !vz:
		return true, ""
	}
	if r := model.AppleNodeReason(m.Spec.Apple, n.Metadata.Labels); r != "" {
		return false, r
	}
	if m.Spec.Apple == nil {
		return true, ""
	}
	for _, name := range m.Spec.Apple.Networks {
		net, ok := topo.MacNetworks[m.Namespace()+"/"+name]
		if !ok {
			return false, fmt.Sprintf("MacNetwork %s not found", name)
		}
		if _, member := net.Status.Member(n.Metadata.Name); !member {
			return false, fmt.Sprintf("not a member of MacNetwork %s", name)
		}
	}
	return true, ""
}
