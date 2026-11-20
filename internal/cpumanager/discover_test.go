// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package cpumanager

import (
	"os"
	"path/filepath"
	"testing"
)

func writeInputs(t *testing.T, online, state string) Input {
	t.Helper()
	dir := t.TempDir()
	in := Input{OnlinePath: filepath.Join(dir, "online"), StatePath: filepath.Join(dir, "cpu_manager_state")}
	if err := os.WriteFile(in.OnlinePath, []byte(online), 0o644); err != nil {
		t.Fatal(err)
	}
	if state != "" {
		if err := os.WriteFile(in.StatePath, []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

func TestDiscoverStaticSubtractsExclusiveAndReservedNotDefault(t *testing.T) {
	// defaultCpuSet is the shared pool: every CPU not exclusively assigned.
	in := writeInputs(t, "0-7\n", `{"policyName":"static","defaultCpuSet":"0-1,4-7","entries":{"p":{"c":"2-3"}}}`)
	in.Reserved = "0-1"
	got, err := Discover(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused != "" || got.List != "4-7" {
		t.Fatalf("got %+v, want 4-7", got)
	}
}

func TestDiscoverNonContiguousIsLabelSafe(t *testing.T) {
	in := writeInputs(t, "0-11\n", `{"policyName":"static","defaultCpuSet":"0-3,6-11","entries":{"p":{"c":"4-5"}}}`)
	in.Reserved = "0-1"
	got, err := Discover(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused != "" || got.List != "2-3_6-11" {
		t.Fatalf("got %+v, want 2-3_6-11", got)
	}
}

func TestDiscoverNonePolicyKeepsReservedOff(t *testing.T) {
	in := writeInputs(t, "0-11\n", `{"policyName":"none","defaultCpuSet":"","checksum":1}`)
	in.Reserved = "0-1"
	got, err := Discover(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Refused != "" || got.List != "2-11" {
		t.Fatalf("got %+v, want 2-11", got)
	}
}

func TestDiscoverRefusesWithoutProof(t *testing.T) {
	cases := map[string]Input{
		"no reserved": writeInputs(t, "0-3\n", `{"policyName":"none","defaultCpuSet":""}`),
		"no state": func() Input {
			in := writeInputs(t, "0-3\n", "")
			in.Reserved = "0"
			return in
		}(),
		"bad reserved": func() Input {
			in := writeInputs(t, "0-3\n", `{"policyName":"none","defaultCpuSet":""}`)
			in.Reserved = "0-x"
			return in
		}(),
		"bad entry": func() Input {
			in := writeInputs(t, "0-3\n", `{"policyName":"static","defaultCpuSet":"0-1","entries":{"p":{"c":"two"}}}`)
			in.Reserved = "0"
			return in
		}(),
	}
	for name, in := range cases {
		got, err := Discover(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Refused == "" || got.List != "" {
			t.Fatalf("%s: got %+v, want refused", name, got)
		}
	}
}

func TestLabelPatchDoesNotClobberOperator(t *testing.T) {
	if LabelPatch("2-5", "", "4-7", false, false) != nil {
		t.Fatal("operator label was overwritten")
	}
	got := LabelPatch("2-5", SourceDiscovered, "4-7", true, false)
	labels := got["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["kairon.zyvor.dev/pinnable-cpus"] != "4-7" {
		t.Fatalf("patch = %#v", got)
	}
	if LabelPatch("4-7", SourceDiscovered, "", true, true) == nil {
		t.Fatal("refused discovery should clear an owned label")
	}
}
