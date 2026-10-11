// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { buildLanes, canMigrateTo, nodeOf, TopoPod, UNSCHEDULED } from './topology';
import { KaironNode, Machine } from '../types';

const node = (name: string, ready: string): KaironNode =>
  ({ metadata: { name }, status: { conditions: [{ type: 'Ready', status: ready }] } }) as KaironNode;
const vm = (name: string, nodeName: string | undefined, phase = 'Running'): Machine => ({
  metadata: { name, namespace: 'default' },
  spec: { nodeName, image: { path: '' }, resources: { cpu: '1', memory: '1Gi' } },
  status: { phase },
});

describe('buildLanes', () => {
  const nodes = [node('w2', 'True'), node('w1', 'True'), node('idle', 'False')];
  const pods: TopoPod[] = [
    { namespace: 'kairon-system', name: 'agent', phase: 'Running', node: 'w1', ready: true },
    { namespace: 'x', name: 'pending', phase: 'Pending', ready: false },
  ];
  const lanes = buildLanes([vm('a', 'w1'), vm('b', 'w2'), vm('c', undefined, 'Pending')], nodes, pods);

  it('sorts by node with unscheduled last', () => {
    expect(lanes.map((l) => l.node)).toEqual(['idle', 'w1', 'w2', UNSCHEDULED]);
  });
  it('places machines and pods', () => {
    const w1 = lanes.find((l) => l.node === 'w1')!;
    expect(w1.machines.map((m) => m.metadata.name)).toEqual(['a']);
    expect(w1.pods).toHaveLength(1);
    expect(lanes.find((l) => l.node === UNSCHEDULED)!.machines[0].metadata.name).toBe('c');
  });
  it('keeps empty known nodes and hides an empty unscheduled lane', () => {
    expect(buildLanes([], nodes, []).map((l) => l.node)).toEqual(['idle', 'w1', 'w2']);
  });
  it('adds lanes for nodes only seen on a machine', () => {
    expect(buildLanes([vm('z', 'ghost')], [], []).map((l) => l.node)).toEqual(['ghost']);
  });
});

describe('canMigrateTo', () => {
  const lane = (n: string, ready: string) => ({ node: n, ready, machines: [], pods: [] });
  it('requires a Running machine and a different Ready node', () => {
    expect(canMigrateTo(vm('a', 'w1'), lane('w2', 'True'))).toBe(true);
    expect(canMigrateTo(vm('a', 'w1'), lane('w1', 'True'))).toBe(false);
    expect(canMigrateTo(vm('a', 'w1'), lane('w2', 'False'))).toBe(false);
    expect(canMigrateTo(vm('a', 'w1', 'Stopped'), lane('w2', 'True'))).toBe(false);
    expect(canMigrateTo(vm('a', 'w1'), lane(UNSCHEDULED, 'True'))).toBe(false);
  });
  it('reads node from status first', () => {
    const m = vm('a', 'spec');
    m.status!.nodeName = 'status';
    expect(nodeOf(m)).toBe('status');
  });
});
