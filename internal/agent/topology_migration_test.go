// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestRetargetRecordNUMA(t *testing.T) {
	raw := json.RawMessage(`{"id":"x","request":{"vcpus":4,"numa":[` +
		`{"host_node":0,"vcpus":[0,1],"host_cpus":[0,1],"memory_mib":1024},` +
		`{"host_node":1,"vcpus":[2,3],"host_cpus":[8,9],"memory_mib":1024}]}}`)
	alloc := &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{
		{ID: 1, CPUs: "10-11", MemoryMiB: 1024},
		{ID: 0, CPUs: "4,6", MemoryMiB: 1024},
	}}
	out, err := retargetRecordNUMA(raw, alloc)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID      string `json:"id"`
		Request struct {
			VCPUs int `json:"vcpus"`
			NUMA  []struct {
				HostNode uint32   `json:"host_node"`
				VCPUs    []uint32 `json:"vcpus"`
				HostCPUs []uint32 `json:"host_cpus"`
				Memory   uint64   `json:"memory_mib"`
			} `json:"numa"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "x" || got.Request.VCPUs != 4 || len(got.Request.NUMA) != 2 {
		t.Fatalf("unrelated fields changed: %s", out)
	}
	n0, n1 := got.Request.NUMA[0], got.Request.NUMA[1]
	if n0.HostNode != 1 || !reflect.DeepEqual(n0.HostCPUs, []uint32{10, 11}) || !reflect.DeepEqual(n0.VCPUs, []uint32{0, 1}) {
		t.Fatalf("guest node 0: %+v", n0)
	}
	if n1.HostNode != 0 || !reflect.DeepEqual(n1.HostCPUs, []uint32{4, 6}) || n1.Memory != 1024 {
		t.Fatalf("guest node 1: %+v", n1)
	}

	if same, err := retargetRecordNUMA(raw, nil); err != nil || string(same) != string(raw) {
		t.Fatalf("nil allocation must pass the record through: %v", err)
	}
	if _, err := retargetRecordNUMA(json.RawMessage(`{"request":{}}`), alloc); err == nil || !strings.Contains(err.Error(), "TopologyShapeUnavailable") {
		t.Fatalf("shape mismatch must fail: %v", err)
	}
}
