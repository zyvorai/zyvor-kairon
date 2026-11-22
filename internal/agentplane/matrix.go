// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import "fmt"

// RequiredMigrationCases is the v0.7 claim. A green README line needs
// every one of these passed on real hardware; this function only
// checks a report.
var RequiredMigrationCases = []string{
	"cold",
	"live",
	"live-ebpf",
	"source-failure",
	"controller-failover",
}

type MatrixCase struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

func MigrationClaim(cases []MatrixCase) error {
	got := map[string]bool{}
	for _, c := range cases {
		got[c.Name] = c.Passed
	}
	var missing []string
	for _, name := range RequiredMigrationCases {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("migration claim is not green, missing or failed: %s", join(missing))
	}
	return nil
}

func join(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
