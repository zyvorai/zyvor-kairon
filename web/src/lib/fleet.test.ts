// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { factsOf, isStale } from './fleet';

describe('fleet facts', () => {
  const now = Date.parse('2026-10-11T12:00:00Z');
  it('is empty without status', () => {
    expect(factsOf(undefined, now)).toEqual([]);
  });
  it('lists set fields in order and sorts totals', () => {
    const facts = factsOf(
      {
        desiredReplicas: 3,
        children: [{ phase: 'Running' }, { phase: 'Pending' }],
        lastActionTime: '2026-10-11T11:55:00Z',
        dataAgeSeconds: 12.4,
        totals: { vms: 4, cost: 1.5 },
      },
      now,
    );
    expect(facts.map((f) => `${f.label}=${f.value}`)).toEqual([
      'Desired replicas=3',
      'Children=1/2 ready',
      'Last action=5m ago',
      'Data age=12 s',
      'cost=1.50',
      'vms=4',
    ]);
  });
  it('flags stale telemetry', () => {
    expect(isStale({ dataAgeSeconds: 301 })).toBe(true);
    expect(isStale({ dataAgeSeconds: 10 })).toBe(false);
    expect(isStale(undefined)).toBe(false);
  });
});
