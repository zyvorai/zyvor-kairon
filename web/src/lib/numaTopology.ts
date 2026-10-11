// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// GET /api/v1/nodes/{node}/topology (internal/uiapi/nodetopology.go).
export interface TopologyDevice {
  bdf: string;
  class: string;
  vendor: string;
  device: string;
  numaNode?: number;
  pcieRoot: string;
  physfn?: string;
  totalVFs?: number;
  vramGiB?: number;
  allocatable: boolean;
  allocated?: boolean;
}

export interface TopologyNUMANode {
  id: number;
  cpus: string;
  memoryMiB: number;
  freeMemoryMiB?: number;
  hugepages?: { size: string; total: number; free: number }[];
}

export interface TopologyAllocation {
  numaNodes?: { id: number; cpus: string; memoryMiB: number; hugepages?: number }[];
  devices?: { name: string; bdf: string; numaNode?: number; pcieRoot?: string }[];
}

export interface NodeTopologyResponse {
  node: string;
  topology: { numaNodes?: TopologyNUMANode[]; devices?: TopologyDevice[]; numaBinding?: boolean; observedAt?: string } | null;
  allocations: {
    namespace: string;
    name: string;
    policy: string;
    allocation: TopologyAllocation;
    applied?: { message?: string };
  }[];
}

export interface NUMARow {
  id: number;
  cpus: string;
  memoryMiB: number;
  devices: (TopologyDevice & { owner?: string })[];
  machines: { ref: string; cpus: string; memoryMiB: number; warning?: string }[];
}

// numaRows groups devices and Machine allocations under their NUMA node;
// devices without one land in a trailing row with id -1.
export function numaRows(r: NodeTopologyResponse): NUMARow[] {
  const owner: Record<string, string> = {};
  const rows: NUMARow[] = (r.topology?.numaNodes ?? []).map((n) => ({ id: n.id, cpus: n.cpus, memoryMiB: n.memoryMiB, devices: [], machines: [] }));
  const byID = new Map(rows.map((row) => [row.id, row]));
  for (const a of r.allocations) {
    const ref = `${a.namespace}/${a.name}`;
    for (const d of a.allocation.devices ?? []) owner[d.bdf] = ref;
    for (const n of a.allocation.numaNodes ?? []) {
      byID.get(n.id)?.machines.push({ ref, cpus: n.cpus, memoryMiB: n.memoryMiB, warning: a.applied?.message || undefined });
    }
  }
  let remote: NUMARow | undefined;
  for (const d of r.topology?.devices ?? []) {
    const dev = { ...d, owner: owner[d.bdf] };
    const row = d.numaNode === undefined ? (remote ??= { id: -1, cpus: '', memoryMiB: 0, devices: [], machines: [] }) : byID.get(d.numaNode);
    row?.devices.push(dev);
  }
  return remote ? [...rows, remote] : rows;
}
