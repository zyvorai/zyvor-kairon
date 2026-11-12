// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Upper bounds keep float-to-integer conversion defined (NaN, Inf and
// out-of-range floats convert to arbitrary values in Go, which would read as
// a zero request and skip every capacity check) and leave headroom for the
// scheduler's sums.
const (
	maxVCPUs     = 1 << 20
	maxMemoryMiB = 1 << 40
)

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func ParseVCPUs(v string) (uint32, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("cpu quantity is empty")
	}
	if strings.HasSuffix(v, "m") {
		milli, err := strconv.ParseFloat(strings.TrimSuffix(v, "m"), 64)
		if err != nil || milli <= 0 || !finite(milli) || math.Ceil(milli/1000) > maxVCPUs {
			return 0, fmt.Errorf("invalid cpu quantity %q", v)
		}
		return uint32(math.Ceil(milli / 1000)), nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || !finite(n) || math.Ceil(n) > maxVCPUs {
		return 0, fmt.Errorf("invalid cpu quantity %q", v)
	}
	return uint32(math.Ceil(n)), nil
}

// ParseBytes parses a Kubernetes-style storage quantity into bytes.
func ParseBytes(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("size quantity is empty")
	}
	units := []struct {
		suffix string
		bytes  float64
	}{
		{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"K", 1e3}, {"k", 1e3},
	}
	num, mult := v, 1.0
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			mult = u.bytes
			num = strings.TrimSpace(strings.TrimSuffix(v, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 || !finite(n) || n*mult >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size quantity %q", v)
	}
	return int64(math.Ceil(n * mult)), nil
}

func ParseMemoryMiB(v string) (uint64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("memory quantity is empty")
	}
	units := []struct {
		suffix string
		bytes  float64
	}{
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"Mi", 1024 * 1024},
		{"Ki", 1024},
		{"T", 1000 * 1000 * 1000 * 1000},
		{"G", 1000 * 1000 * 1000},
		{"M", 1000 * 1000},
		{"K", 1000},
	}
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, u.suffix)), 64)
			if err != nil || n <= 0 || !finite(n) || n*u.bytes/(1024*1024) > maxMemoryMiB {
				return 0, fmt.Errorf("invalid memory quantity %q", v)
			}
			return uint64(math.Ceil(n * u.bytes / (1024 * 1024))), nil
		}
	}
	bytes, err := strconv.ParseFloat(v, 64)
	if err != nil || bytes <= 0 || !finite(bytes) || bytes/(1024*1024) > maxMemoryMiB {
		return 0, fmt.Errorf("invalid memory quantity %q", v)
	}
	return uint64(math.Ceil(bytes / (1024 * 1024))), nil
}

// ParseDiskSizeGiB parses a disk quantity (60Gi, 1Ti, 100G) and rounds up to
// whole GiB, the unit FluxVM sizes root disks in.
func ParseDiskSizeGiB(v string) (uint64, error) {
	mib, err := ParseMemoryMiB(v)
	if err != nil {
		return 0, fmt.Errorf("invalid disk size %q", strings.TrimSpace(v))
	}
	return (mib + 1023) / 1024, nil
}
