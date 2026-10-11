// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/fluxvm"
	"github.com/zyvorai/kairon/internal/model"
)

func TestBuildAppliedTopology(t *testing.T) {
	alloc := &model.TopologyAllocation{NUMANodes: []model.AllocatedNUMANode{{ID: 0, CPUs: "2-3", MemoryMiB: 2048}}}
	var st fluxvm.VMNUMAStatus
	if err := json.Unmarshal([]byte(`{"nodes":[{"host_node":0,"vcpus":[0,1],"host_cpus":[2,3],"memory_mib":2048}],
	  "vcpus":[{"vcpu":0,"thread_id":11,"host_node":0,"host_cpus":[2,3]},{"vcpu":1,"thread_id":12,"host_node":0,"host_cpus":[2,3]}],
	  "memdevs":[{"id":"ram-node0","size_mib":2048,"host_nodes":[0],"policy":"bind"}]}`), &st); err != nil {
		t.Fatal(err)
	}
	got := buildAppliedTopology(alloc, &st, []string{"0000:3b:00.0"})
	n := got.NUMANodes[0]
	if got.Message != "" || n.HostNode != 0 || n.VCPUs != "0-1" || n.HostCPUs != "2-3" || n.Policy != "bind" || len(got.Devices) != 1 {
		t.Fatalf("%+v", got)
	}

	st.VCPUs[1].HostCPUs = []uint32{2, 3, 4}
	if got := buildAppliedTopology(alloc, &st, nil); !strings.Contains(got.Message, "best-effort") {
		t.Fatalf("drifted vCPU affinity must be reported: %+v", got)
	}
	st.Memdevs[0].Policy = "default"
	if got := buildAppliedTopology(alloc, &st, nil); !strings.Contains(got.Message, "not bind") {
		t.Fatalf("unbound memory must be reported: %+v", got)
	}
}

func TestSameTopologyIgnoresFreeCounters(t *testing.T) {
	a := model.NodeTopologyStatus{NUMANodes: []model.TopologyNUMANode{{ID: 0, CPUs: "0-3", FreeMemoryMiB: 10, Hugepages: []model.TopologyHugepages{{Size: "2Mi", Total: 8, Free: 8}}}}}
	b := model.NodeTopologyStatus{NUMANodes: []model.TopologyNUMANode{{ID: 0, CPUs: "0-3", FreeMemoryMiB: 99, Hugepages: []model.TopologyHugepages{{Size: "2Mi", Total: 8, Free: 1}}}}}
	if !sameTopology(a, b) {
		t.Fatal("free memory and free hugepages alone must not trigger a write")
	}
	b.NUMANodes[0].Hugepages[0].Total = 16
	if sameTopology(a, b) {
		t.Fatal("a hugepage pool resize must trigger a write")
	}
	if a.NUMANodes[0].FreeMemoryMiB != 10 {
		t.Fatal("sameTopology must not modify its arguments")
	}
}
