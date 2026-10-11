// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { Machine } from '../types';

export type NetHealth = 'ok' | 'warn' | 'idle';

// netHealth: an attached dataplane with its policy synced is healthy; an
// attached one still waiting on policy sync needs attention; no dataplane
// status at all (stopped, or not edge-managed) is idle.
export function netHealth(m: Machine): NetHealth {
  const dp = m.status?.network?.dataplane;
  if (!dp) return 'idle';
  if (dp.attached && dp.policySynced) return 'ok';
  return 'warn';
}

export interface NetSummary {
  total: number;
  attached: number;
  policySynced: number;
  antiSpoof: number;
  needsAttention: number;
  byPolicy: { policy: string; count: number }[];
}

export function summarizeNetwork(machines: Machine[]): NetSummary {
  const s: NetSummary = { total: machines.length, attached: 0, policySynced: 0, antiSpoof: 0, needsAttention: 0, byPolicy: [] };
  const policies = new Map<string, number>();
  for (const m of machines) {
    const n = m.status?.network;
    if (n?.dataplane?.attached) s.attached++;
    if (n?.dataplane?.policySynced) s.policySynced++;
    if (n?.edge?.antiSpoof) s.antiSpoof++;
    if (netHealth(m) === 'warn') s.needsAttention++;
    const p = n?.edge?.policyName;
    if (p) policies.set(p, (policies.get(p) ?? 0) + 1);
  }
  s.byPolicy = [...policies].map(([policy, count]) => ({ policy, count })).sort((a, b) => b.count - a.count || a.policy.localeCompare(b.policy));
  return s;
}
