// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package hotplugpersist

import (
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestBootUsesAppliedWhenLarger(t *testing.T) {
	got, err := Boot(model.ResourceSpec{CPU: "2", Memory: "2Gi", MaxCPU: "8", MaxMemory: "16Gi"}, 4, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got.VCPUs != 4 || got.MemoryMiB != 4096 || got.CPU != "4" || got.Memory != "4096Mi" {
		t.Fatalf("boot = %+v", got)
	}
}

func TestBootClampsToMax(t *testing.T) {
	got, err := Boot(model.ResourceSpec{CPU: "2", Memory: "2Gi", MaxCPU: "4", MaxMemory: "4Gi"}, 8, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if got.VCPUs != 4 || got.MemoryMiB != 4096 {
		t.Fatalf("boot = %+v, want clamped to max", got)
	}
}

func TestBootKeepsSpecWhenAppliedEmpty(t *testing.T) {
	got, err := Boot(model.ResourceSpec{CPU: "2", Memory: "2Gi"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.VCPUs != 2 || got.MemoryMiB != 2048 {
		t.Fatalf("boot = %+v", got)
	}
}

func TestSpecPatchOnlyWhenOptedInAndGrown(t *testing.T) {
	m := model.Machine{Spec: model.MachineSpec{Resources: model.ResourceSpec{CPU: "2", Memory: "2Gi"}}}
	if SpecPatch(m, 4, 4096) != nil {
		t.Fatal("patch without opt-in")
	}
	m.Metadata.Annotations = map[string]string{AnnPersist: "true"}
	if SpecPatch(m, 2, 2048) != nil {
		t.Fatal("patch when already at spec")
	}
	got := SpecPatch(m, 4, 2048)
	res := got["spec"].(map[string]any)["resources"].(map[string]any)
	if res["cpu"] != "4" || res["memory"] != "2Gi" {
		t.Fatalf("patch = %#v", res)
	}
}
