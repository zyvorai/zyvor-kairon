// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package model

import "testing"

func FuzzParseVCPUs(f *testing.F) {
	for _, seed := range []string{"1", "2", "500m", "0.5", "", "abc", "1e9", "-1", "999999m"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v string) {
		_, _ = ParseVCPUs(v)
	})
}

func FuzzParseMemoryMiB(f *testing.F) {
	for _, seed := range []string{"1Gi", "512Mi", "1024", "1G", "1Ti", "", "xyz", "0Mi", "-1Gi"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v string) {
		_, _ = ParseMemoryMiB(v)
	})
}

func FuzzParseCPUList(f *testing.F) {
	for _, seed := range []string{"0", "0-3", "0,2,4", "1-1", "", "a-b", "0-99999", "3-1"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v string) {
		_, _ = ParseCPUList(v)
	})
}

func FuzzParseIntOrPercent(f *testing.F) {
	for _, seed := range []string{"1", "50%", "100%", "0", "", "x%", "-1", "999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v string) {
		_, _ = ParseIntOrPercent(v, 10)
	})
}

func FuzzValidateVmNetworkPolicy(f *testing.F) {
	f.Add("10.0.0.0/8", "tcp", "80")
	f.Add("", "udp", "0")
	f.Add("not-a-cidr", "sctp", "65535")
	f.Fuzz(func(t *testing.T, cidr, proto, port string) {
		p := VmNetworkPolicy{
			AllowCidrs: []string{cidr},
			AllowPorts: []string{proto + "/" + port},
		}
		_ = ValidateVmNetworkPolicy(p)
	})
}
