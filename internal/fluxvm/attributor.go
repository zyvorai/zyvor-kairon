// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
package fluxvm

// Attributor is the optional hook for process attribution when the
// dataplane dump does not already carry process and RTT fields.
// Wire it as an ebpfedge.Chain only if a lookup API exists that is
// not already on the dump. PacketWolf stays out of this repository.
type Attributor interface {
	// Attribute fills process and latency fields on a flow dump.
	// Implementations must not overwrite a guest PID with a host PID.
	Attribute(dump []byte) ([]byte, error)
}

// NopAttributor is the default. It returns the dump unchanged.
type NopAttributor struct{}

func (NopAttributor) Attribute(dump []byte) ([]byte, error) {
	return dump, nil
}
