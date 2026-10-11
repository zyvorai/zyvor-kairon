// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { barTone, formatBalloon, formatCores, formatMiB, memoryLimitMiB, NodeCapacity, pct } from './capacity';

describe('capacity helpers', () => {
  it('computes percentages and guards zero', () => {
    expect(pct(3000, 4000)).toBe(75);
    expect(pct(5000, 4000)).toBe(125);
    expect(pct(1, 0)).toBeNull();
  });
  it('picks a tone by fullness', () => {
    expect(barTone(null)).toBe('idle');
    expect(barTone(50)).toBe('ok');
    expect(barTone(80)).toBe('warn');
    expect(barTone(95)).toBe('error');
  });
  it('formats cores and memory', () => {
    expect(formatCores(4000)).toBe('4');
    expect(formatCores(2500)).toBe('2.5');
    expect(formatMiB(512)).toBe('512 MiB');
    expect(formatMiB(8192)).toBe('8 GiB');
    expect(formatMiB(1536)).toBe('1.5 GiB');
  });
  it('scales the memory limit by the overcommit ratio', () => {
    const base: NodeCapacity = {
      node: 'n', ready: true, unschedulable: false, taints: 0, machines: 0,
      allocatableCpuMilli: 4000, allocatableMemoryMiB: 8192, requestedCpuMilli: 0, requestedMemoryMiB: 0, knownAllocatable: true,
    };
    expect(memoryLimitMiB(base)).toBe(8192);
    expect(memoryLimitMiB({ ...base, memoryOvercommitRatio: 1.5 })).toBe(12288);
    expect(memoryLimitMiB({ ...base, memoryOvercommitRatio: 0.5 })).toBe(8192);
  });
  it('formats a balloon', () => {
    expect(formatBalloon({ currentMiB: 4096, balloonActualMiB: 1024, minMiB: 2048 })).toBe('3 GiB of 4 GiB, min 2 GiB');
    expect(formatBalloon({ currentMiB: 4096, balloonActualMiB: 0 })).toBe('4 GiB of 4 GiB');
  });
});
