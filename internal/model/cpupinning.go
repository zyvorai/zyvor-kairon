// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PinnableCPUsLabel is an opt-in, operator-asserted node label naming the
// real host CPU numbers a node makes available for exclusive
// spec.resources.cpuPinning allocation -- the same "operator asserts a
// fact Kairon has no other way to know" shape StorageDomainLabel/
// NetworkDomainLabel/VFIODevicesLabel already established
// (internal/controller/preflight.go). Absent or empty on a node means
// "no pinnable cores here" -- fail-closed, the same posture an empty
// KAIRON_VFIO_ALLOWLIST already has for VFIO passthrough, not "assume
// every core is free."
//
// Value syntax is Linux's cpuset.cpus list format with "_" between
// entries (e.g. "2-15" or "2-3_6-11"): a label value cannot contain ",".
// It must also fit the 63-character label limit; see FormatCPULabel.
const PinnableCPUsLabel = "kairon.zyvor.dev/pinnable-cpus"

// ParseCPUList parses a Linux cpuset.cpus-style list ("2-15",
// "2,3,4-8,20", or the label form "2_3_4-8_20") into an explicit,
// deduplicated, ascending slice of CPU numbers. An empty string returns
// an empty (not nil) slice, matching "asserted, but pins nothing" rather
// than an error.
func ParseCPUList(raw string) ([]uint32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []uint32{}, nil
	}
	seen := map[uint32]struct{}{}
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '_' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			start, err := strconv.ParseUint(lo, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range %q: %w", part, err)
			}
			end, err := strconv.ParseUint(hi, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU range %q: %w", part, err)
			}
			if end < start {
				return nil, fmt.Errorf("invalid CPU range %q: end before start", part)
			}
			for v := start; v <= end; v++ {
				seen[uint32(v)] = struct{}{}
			}
		} else {
			v, err := strconv.ParseUint(part, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid CPU number %q: %w", part, err)
			}
			seen[uint32(v)] = struct{}{}
		}
	}
	out := make([]uint32, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// FormatCPULabel renders an ascending CPU list as a PinnableCPUsLabel
// value: ranges joined by "_". It errors when the result exceeds the
// 63-character label limit.
func FormatCPULabel(cpus []uint32) (string, error) {
	out := strings.Join(cpuRanges(cpus), "_")
	if len(out) > 63 {
		return "", fmt.Errorf("cpu list %q exceeds the 63-character label limit", out)
	}
	return out, nil
}

// FormatCPUList renders an ascending CPU list in kernel form ("0-3,8").
func FormatCPUList(cpus []uint32) string {
	return strings.Join(cpuRanges(cpus), ",")
}

func cpuRanges(cpus []uint32) []string {
	if len(cpus) == 0 {
		return nil
	}
	var parts []string
	start, prev := cpus[0], cpus[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.FormatUint(uint64(start), 10))
			return
		}
		parts = append(parts, strconv.FormatUint(uint64(start), 10)+"-"+strconv.FormatUint(uint64(prev), 10))
	}
	for _, cpu := range cpus[1:] {
		if cpu == prev+1 {
			prev = cpu
			continue
		}
		flush()
		start, prev = cpu, cpu
	}
	flush()
	return parts
}
