// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { KaironNode, Machine } from '../types';
import { nodeReadyStatus } from './phase';

export interface TopoPod {
  namespace: string;
  name: string;
  phase: string;
  node?: string;
  ready: boolean;
  component?: string;
}

export interface Lane {
  node: string; // '' is the unscheduled lane
  ready: string;
  machines: Machine[];
  pods: TopoPod[];
}

export function nodeOf(m: Machine): string {
  return m.status?.nodeName || m.spec.nodeName || '';
}

export const UNSCHEDULED = '';

// buildLanes puts every Machine and pod under the node it runs on. Known
// nodes always get a lane (even empty), nodes only referenced by a Machine
// or pod get one too, and unplaced Machines land in the UNSCHEDULED lane,
// which is last. Lanes sort by node name.
export function buildLanes(machines: Machine[], nodes: KaironNode[], pods: TopoPod[]): Lane[] {
  const lanes = new Map<string, Lane>();
  const lane = (node: string, ready = 'Unknown'): Lane => {
    let l = lanes.get(node);
    if (!l) {
      l = { node, ready, machines: [], pods: [] };
      lanes.set(node, l);
    }
    return l;
  };
  for (const n of nodes) lane(n.metadata.name, nodeReadyStatus(n)).ready = nodeReadyStatus(n);
  for (const m of machines) lane(nodeOf(m)).machines.push(m);
  for (const p of pods) if (p.node) lane(p.node).pods.push(p);
  return [...lanes.values()]
    .filter((l) => l.node !== UNSCHEDULED || l.machines.length > 0)
    .sort((a, b) => (a.node === UNSCHEDULED ? 1 : b.node === UNSCHEDULED ? -1 : a.node.localeCompare(b.node)));
}

// canMigrateTo: a Running Machine may be dropped on a different, Ready node.
export function canMigrateTo(m: Machine, lane: Lane): boolean {
  return m.status?.phase === 'Running' && lane.node !== UNSCHEDULED && lane.node !== nodeOf(m) && lane.ready === 'True';
}
