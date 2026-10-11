// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { numaRows } from './numaTopology';

describe('numaRows', () => {
  it('groups devices and Machines under their NUMA node', () => {
    const rows = numaRows({
      node: 'w1',
      topology: {
        numaNodes: [
          { id: 0, cpus: '0-7', memoryMiB: 16384 },
          { id: 1, cpus: '8-15', memoryMiB: 16384 },
        ],
        devices: [
          { bdf: '0000:3b:00.0', class: 'gpu', vendor: '10de', device: '2330', numaNode: 1, pcieRoot: 'pci0000:3a', allocatable: true },
          { bdf: '0000:d8:00.0', class: 'nvme', vendor: '8086', device: '0a54', pcieRoot: 'pci0000:d7', allocatable: false },
        ],
      },
      allocations: [
        {
          namespace: 'ml',
          name: 'train',
          policy: 'single-numa-node',
          allocation: { numaNodes: [{ id: 1, cpus: '8-11', memoryMiB: 4096 }], devices: [{ name: 'gpu', bdf: '0000:3b:00.0' }] },
          applied: { message: 'vCPU pinning drifted' },
        },
      ],
    });
    expect(rows.map((r) => r.id)).toEqual([0, 1, -1]);
    expect(rows[1].devices[0].owner).toBe('ml/train');
    expect(rows[1].machines).toEqual([{ ref: 'ml/train', cpus: '8-11', memoryMiB: 4096, warning: 'vCPU pinning drifted' }]);
    expect(rows[2].devices[0].bdf).toBe('0000:d8:00.0');
  });

  it('returns nothing for a node without NodeTopology', () => {
    expect(numaRows({ node: 'w2', topology: null, allocations: [] })).toEqual([]);
  });
});
