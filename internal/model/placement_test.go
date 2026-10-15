// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import "testing"

func TestValidatePlacementSpread(t *testing.T) {
	spec := func(skew int32, when string) MachineSpec {
		return MachineSpec{Placement: PlacementSpec{TopologySpreadConstraints: []TopologySpreadConstraint{
			{TopologyKey: "zone", MaxSkew: skew, WhenUnsatisfiable: when},
		}}}
	}
	for _, tc := range []struct {
		name string
		s    MachineSpec
		ok   bool
	}{
		{"none", MachineSpec{}, true},
		{"hard skew 1", spec(1, WhenUnsatisfiableDoNotSchedule), true},
		{"soft default, unset skew", spec(0, ""), true},
		{"soft anyway, unset skew", spec(0, WhenUnsatisfiableScheduleAnyway), true},
		{"hard skew 0", spec(0, WhenUnsatisfiableDoNotSchedule), false},
		{"negative skew", spec(-1, WhenUnsatisfiableScheduleAnyway), false},
		{"unknown policy", spec(1, "Sometimes"), false},
	} {
		if err := ValidatePlacement(tc.s); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
}
