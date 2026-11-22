// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package agentplane

import (
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// ProjectPinnable subtracts reserved CPUs from a cpuset.cpus.effective
// list and returns the label value operators paste today. It does not
// read the host; kairon-node passes the text.
func ProjectPinnable(effective, reserved string) (string, error) {
	all, err := model.ParseCPUList(effective)
	if err != nil {
		return "", fmt.Errorf("effective: %w", err)
	}
	hold, err := model.ParseCPUList(reserved)
	if err != nil {
		return "", fmt.Errorf("reserved: %w", err)
	}
	drop := map[uint32]struct{}{}
	for _, c := range hold {
		drop[c] = struct{}{}
	}
	var keep []uint32
	for _, c := range all {
		if _, ok := drop[c]; ok {
			continue
		}
		keep = append(keep, c)
	}
	return model.FormatCPULabel(keep)
}
