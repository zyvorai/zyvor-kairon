// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
package kaironctl

import "fmt"

// ScaleHint returns the kubectl command to scale a MachineSet.
func ScaleHint(name string, replicas int32) string {
	if replicas < 0 {
		return "replicas must be >= 0"
	}
	return fmt.Sprintf("kubectl scale machineset %s --replicas=%d", name, replicas)
}
