// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

package conversion

import (
	"encoding/json"
	"fmt"
)

const (
	machineQuotaV1Alpha1 = "kairon.zyvor.dev/v1alpha1"
	machineQuotaV1       = "kairon.zyvor.dev/v1"
)

// machineQuotaSpecRenames/machineQuotaStatusRenames are v1alpha1's field
// names -> v1's, the worked example this scaffold proves itself
// against: v1 drops the redundant "Total" qualifier
// (MachineQuota.spec.maxTotalCpu/maxTotalMemory,
// status.usedTotalCpuCores/usedTotalMemoryMiB) -- a quota's cap is
// inherently a total, the word never carried real information.
// maxMachines/usedMachines are deliberately absent from both maps: not
// every field needs to move just because the version does, and a
// converter that only touches what actually changed is the realistic
// shape a future real conversion will take too.
var (
	machineQuotaSpecRenames   = map[string]string{"maxTotalCpu": "maxCpu", "maxTotalMemory": "maxMemory"}
	machineQuotaStatusRenames = map[string]string{"usedTotalCpuCores": "usedCpuCores", "usedTotalMemoryMiB": "usedMemoryMiB"}
)

// ConvertMachineQuota is the worked example proving internal/conversion's
// machinery end-to-end -- see the package doc comment. The renames below
// are hypothetical: the served v1, v1beta1 and v1alpha1 MachineQuota
// schemas are identical (see docs/guides/crd-versioning.md), so this is
// ready to wire in the moment a real schema change lands, not something
// built from scratch that day.
func ConvertMachineQuota(obj map[string]any, toVersion string) (map[string]any, error) {
	from, _ := obj["apiVersion"].(string)
	if from == "" {
		return nil, fmt.Errorf("object has no apiVersion")
	}
	if from == toVersion {
		return obj, nil
	}
	out, err := deepCopyObject(obj)
	if err != nil {
		return nil, fmt.Errorf("copy object: %w", err)
	}
	switch {
	case from == machineQuotaV1Alpha1 && toVersion == machineQuotaV1:
		renameFieldsIn(out, "spec", machineQuotaSpecRenames)
		renameFieldsIn(out, "status", machineQuotaStatusRenames)
	case from == machineQuotaV1 && toVersion == machineQuotaV1Alpha1:
		renameFieldsIn(out, "spec", invertRenames(machineQuotaSpecRenames))
		renameFieldsIn(out, "status", invertRenames(machineQuotaStatusRenames))
	default:
		return nil, fmt.Errorf("unsupported MachineQuota conversion %s -> %s", from, toVersion)
	}
	out["apiVersion"] = toVersion
	return out, nil
}

// renameFieldsIn moves obj[section][from] to obj[section][to] for every
// entry in renames, for whichever of section's keys are actually present
// -- a field a particular object never set (every MachineQuota field is
// optional) is simply not there to rename, not an error.
func renameFieldsIn(obj map[string]any, section string, renames map[string]string) {
	sub, ok := obj[section].(map[string]any)
	if !ok {
		return
	}
	for from, to := range renames {
		if v, present := sub[from]; present {
			delete(sub, from)
			sub[to] = v
		}
	}
}

func invertRenames(renames map[string]string) map[string]string {
	inverted := make(map[string]string, len(renames))
	for from, to := range renames {
		inverted[to] = from
	}
	return inverted
}

// deepCopyObject returns a copy of obj sharing no nested maps with it --
// renameFieldsIn mutates in place, and obj may be reused by the caller
// (Handler decodes it fresh per call today, but Converter is also called
// directly in tests against shared fixtures). A JSON marshal/unmarshal
// round trip is the simplest correct way to deep-copy an arbitrary
// map[string]any tree; these objects are small enough that its cost is
// irrelevant.
func deepCopyObject(obj map[string]any) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
