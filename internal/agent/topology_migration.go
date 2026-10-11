// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"fmt"

	"github.com/zyvorai/kairon/internal/model"
)

// retargetRecordNUMA rewrites request.numa of the source FluxVM record the
// receiver launches from, so guest node i lands on the target's host node
// and CPUs from alloc. Guest vCPUs and memory per node stay as they are:
// the guest's NUMA layout cannot change under a live migration.
func retargetRecordNUMA(raw json.RawMessage, alloc *model.TopologyAllocation) (json.RawMessage, error) {
	if alloc == nil || len(alloc.NUMANodes) == 0 {
		return raw, nil
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("decode source record: %w", err)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(record["request"], &request); err != nil {
		return nil, fmt.Errorf("decode source record request: %w", err)
	}
	var numa []map[string]json.RawMessage
	if len(request["numa"]) > 0 {
		if err := json.Unmarshal(request["numa"], &numa); err != nil {
			return nil, fmt.Errorf("decode source record numa: %w", err)
		}
	}
	if len(numa) != len(alloc.NUMANodes) {
		return nil, fmt.Errorf("TopologyShapeUnavailable: source runtime has %d guest NUMA nodes, target allocation has %d", len(numa), len(alloc.NUMANodes))
	}
	for i, n := range alloc.NUMANodes {
		cpus, err := model.ParseCPUList(n.CPUs)
		if err != nil {
			return nil, fmt.Errorf("target allocation numa%d: %w", n.ID, err)
		}
		numa[i]["host_node"], _ = json.Marshal(n.ID)
		numa[i]["host_cpus"], _ = json.Marshal(cpus)
	}
	var err error
	if request["numa"], err = json.Marshal(numa); err != nil {
		return nil, err
	}
	if record["request"], err = json.Marshal(request); err != nil {
		return nil, err
	}
	return json.Marshal(record)
}
