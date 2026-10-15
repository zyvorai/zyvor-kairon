// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "fmt"

// ValidatePlacement rejects topology spread constraints the scheduler can
// never satisfy. A DoNotSchedule constraint with maxSkew below 1 can only
// be met when every Machine lands in a single domain, so with two or more
// domains the Machine would sit Pending forever; Kubernetes rejects the same
// shape at admission.
func ValidatePlacement(s MachineSpec) error {
	for i, c := range s.Placement.TopologySpreadConstraints {
		switch c.WhenUnsatisfiable {
		case "", WhenUnsatisfiableDoNotSchedule, WhenUnsatisfiableScheduleAnyway:
		default:
			return fmt.Errorf("spec.placement.topologySpreadConstraints[%d].whenUnsatisfiable %q must be DoNotSchedule or ScheduleAnyway", i, c.WhenUnsatisfiable)
		}
		if c.MaxSkew < 0 {
			return fmt.Errorf("spec.placement.topologySpreadConstraints[%d].maxSkew %d must not be negative", i, c.MaxSkew)
		}
		if c.WhenUnsatisfiable == WhenUnsatisfiableDoNotSchedule && c.MaxSkew < 1 {
			return fmt.Errorf("spec.placement.topologySpreadConstraints[%d].maxSkew must be at least 1 with whenUnsatisfiable DoNotSchedule", i)
		}
	}
	return nil
}
