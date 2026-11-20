// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package conversion

import "testing"

func TestConvertMachineQuotaAlphaToBetaRenamesFields(t *testing.T) {
	in := map[string]any{
		"apiVersion": machineQuotaV1Alpha1,
		"kind":       "MachineQuota",
		"spec": map[string]any{
			"maxMachines":    float64(10),
			"maxTotalCpu":    "16",
			"maxTotalMemory": "32Gi",
		},
		"status": map[string]any{
			"usedMachines":       float64(3),
			"usedTotalCpuCores":  float64(4),
			"usedTotalMemoryMiB": float64(8192),
		},
	}

	out, err := ConvertMachineQuota(in, machineQuotaV1)
	if err != nil {
		t.Fatalf("ConvertMachineQuota: %v", err)
	}
	if out["apiVersion"] != machineQuotaV1 {
		t.Fatalf("expected apiVersion %q, got %v", machineQuotaV1, out["apiVersion"])
	}

	spec := out["spec"].(map[string]any)
	if _, present := spec["maxTotalCpu"]; present {
		t.Fatal("expected maxTotalCpu to be renamed away, not just copied alongside")
	}
	if spec["maxCpu"] != "16" || spec["maxMemory"] != "32Gi" {
		t.Fatalf("expected renamed spec fields, got %+v", spec)
	}
	if spec["maxMachines"] != float64(10) {
		t.Fatalf("expected maxMachines to pass through unchanged, got %v", spec["maxMachines"])
	}

	status := out["status"].(map[string]any)
	if status["usedCpuCores"] != float64(4) || status["usedMemoryMiB"] != float64(8192) {
		t.Fatalf("expected renamed status fields, got %+v", status)
	}
	if status["usedMachines"] != float64(3) {
		t.Fatalf("expected usedMachines to pass through unchanged, got %v", status["usedMachines"])
	}
}

func TestConvertMachineQuotaBetaToAlphaRenamesFieldsBack(t *testing.T) {
	in := map[string]any{
		"apiVersion": machineQuotaV1,
		"spec":       map[string]any{"maxCpu": "16", "maxMemory": "32Gi"},
		"status":     map[string]any{"usedCpuCores": float64(4), "usedMemoryMiB": float64(8192)},
	}

	out, err := ConvertMachineQuota(in, machineQuotaV1Alpha1)
	if err != nil {
		t.Fatalf("ConvertMachineQuota: %v", err)
	}
	spec := out["spec"].(map[string]any)
	if spec["maxTotalCpu"] != "16" || spec["maxTotalMemory"] != "32Gi" {
		t.Fatalf("expected renamed-back spec fields, got %+v", spec)
	}
	status := out["status"].(map[string]any)
	if status["usedTotalCpuCores"] != float64(4) || status["usedTotalMemoryMiB"] != float64(8192) {
		t.Fatalf("expected renamed-back status fields, got %+v", status)
	}
}

func TestConvertMachineQuotaRoundTripsCleanly(t *testing.T) {
	original := map[string]any{
		"apiVersion": machineQuotaV1Alpha1,
		"metadata":   map[string]any{"name": "q1", "namespace": "prod"},
		"spec":       map[string]any{"maxMachines": float64(5), "maxTotalCpu": "8", "maxTotalMemory": "16Gi"},
		"status":     map[string]any{"usedMachines": float64(1), "usedTotalCpuCores": float64(2), "usedTotalMemoryMiB": float64(4096)},
	}

	toV1, err := ConvertMachineQuota(original, machineQuotaV1)
	if err != nil {
		t.Fatalf("v1alpha1 -> v1: %v", err)
	}
	backToAlpha, err := ConvertMachineQuota(toV1, machineQuotaV1Alpha1)
	if err != nil {
		t.Fatalf("v1 -> v1alpha1: %v", err)
	}

	origSpec, roundSpec := original["spec"].(map[string]any), backToAlpha["spec"].(map[string]any)
	for k, v := range origSpec {
		if roundSpec[k] != v {
			t.Fatalf("spec.%s did not round-trip: started %v, ended %v", k, v, roundSpec[k])
		}
	}
	origStatus, roundStatus := original["status"].(map[string]any), backToAlpha["status"].(map[string]any)
	for k, v := range origStatus {
		if roundStatus[k] != v {
			t.Fatalf("status.%s did not round-trip: started %v, ended %v", k, v, roundStatus[k])
		}
	}
}

func TestConvertMachineQuotaSameVersionIsIdentity(t *testing.T) {
	in := map[string]any{"apiVersion": machineQuotaV1Alpha1, "spec": map[string]any{"maxTotalCpu": "16"}}
	out, err := ConvertMachineQuota(in, machineQuotaV1Alpha1)
	if err != nil {
		t.Fatalf("ConvertMachineQuota: %v", err)
	}
	if out["spec"].(map[string]any)["maxTotalCpu"] != "16" {
		t.Fatalf("expected a same-version conversion to leave fields untouched, got %+v", out)
	}
}

func TestConvertMachineQuotaRejectsUnknownVersions(t *testing.T) {
	if _, err := ConvertMachineQuota(map[string]any{"apiVersion": machineQuotaV1Alpha1}, "kairon.zyvor.dev/v2"); err == nil {
		t.Fatal("expected an error for an unsupported target version")
	}
	if _, err := ConvertMachineQuota(map[string]any{"apiVersion": ""}, machineQuotaV1); err == nil {
		t.Fatal("expected an error for an object with no apiVersion")
	}
}

func TestConvertMachineQuotaDoesNotMutateTheOriginalObject(t *testing.T) {
	in := map[string]any{
		"apiVersion": machineQuotaV1Alpha1,
		"spec":       map[string]any{"maxTotalCpu": "16"},
	}
	if _, err := ConvertMachineQuota(in, machineQuotaV1); err != nil {
		t.Fatalf("ConvertMachineQuota: %v", err)
	}
	if in["apiVersion"] != machineQuotaV1Alpha1 {
		t.Fatalf("expected the original object's apiVersion to be untouched, got %v", in["apiVersion"])
	}
	if _, present := in["spec"].(map[string]any)["maxTotalCpu"]; !present {
		t.Fatal("expected the original object's spec to be untouched (deep copy, not shared)")
	}
}
