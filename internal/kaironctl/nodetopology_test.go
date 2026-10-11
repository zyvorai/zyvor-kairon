// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyvorai/kairon/internal/model"
)

func TestPrintNodeTopologyTree(t *testing.T) {
	zero := 0
	nt := model.NodeTopology{Metadata: model.ObjectMeta{Name: "w1"}}
	nt.Status.NUMABinding = true
	nt.Status.NUMANodes = []model.TopologyNUMANode{
		{ID: 0, CPUs: "0-7", MemoryMiB: 16384, FreeMemoryMiB: 8000, Hugepages: []model.TopologyHugepages{{Size: "1Gi", Total: 8, Free: 6}}},
		{ID: 1, CPUs: "8-15", MemoryMiB: 16384},
	}
	nt.Status.Devices = []model.TopologyDevice{
		{BDF: "0000:3b:00.0", Class: "gpu", Vendor: "10de", Device: "2330", NUMANode: &zero, PCIeRoot: "pci0000:3a", Allocatable: true},
		{BDF: "0000:d8:00.0", Class: "nvme", Vendor: "8086", Device: "0a54", PCIeRoot: "pci0000:d7"},
	}
	m := model.Machine{Metadata: model.ObjectMeta{Name: "train", Namespace: "ml"}}
	m.Spec.NodeName = "w1"
	m.Spec.TopologyAllocation = &model.TopologyAllocation{
		NUMANodes: []model.AllocatedNUMANode{{ID: 0, CPUs: "0-3", MemoryMiB: 4096}},
		Devices:   []model.AllocatedDevice{{Name: "gpu", BDF: "0000:3b:00.0"}},
	}
	var b bytes.Buffer
	printNodeTopology(&b, nt, []model.Machine{m})
	out := b.String()
	for _, want := range []string{
		"w1 (numa binding: yes)",
		"├── numa0  cpus 0-7  mem 16384MiB (8000MiB free)  hugepages 1Gi 6/8 free",
		"│   ├── gpu 0000:3b:00.0 10de:2330 root pci0000:3a -> ml/train",
		"│   └── machine ml/train cpus=0-3 mem=4096MiB",
		"├── numa1  cpus 8-15",
		"└── no NUMA node",
		"    └── nvme 0000:d8:00.0 8086:0a54 root pci0000:d7 [not in VFIO allowlist]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if got := listPath("nodetopologies", false, "default"); got != "/apis/"+crdGroup+"/v1/nodetopologies" {
		t.Fatalf("NodeTopology is cluster-scoped, got %s", got)
	}
}
