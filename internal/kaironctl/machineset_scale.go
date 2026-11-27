// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
package kaironctl

import (
	"fmt"
	"strconv"
)

// ParseReplicas parses a replicas argument. Returns an error if negative.
func ParseReplicas(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid replicas %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("replicas must be >= 0")
	}
	return int32(n), nil
}

// ScaleCommand returns the kubectl scale command for a MachineSet.
func ScaleCommand(name string, replicas int32) string {
	return fmt.Sprintf("kubectl scale machineset %s --replicas=%d", name, replicas)
}
