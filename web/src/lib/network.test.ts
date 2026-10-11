// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { netHealth, summarizeNetwork } from './network';
import { Machine } from '../types';

function vm(name: string, network?: NonNullable<Machine['status']>['network']): Machine {
  return { metadata: { name }, spec: { image: { path: '' }, resources: { cpu: '1', memory: '1Gi' } }, status: { network } };
}

describe('network summary', () => {
  const fleet = [
    vm('a', { dataplane: { attached: true, policySynced: true }, edge: { antiSpoof: true, policyName: 'web' } }),
    vm('b', { dataplane: { attached: true, policySynced: false }, edge: { policyName: 'web' } }),
    vm('c', { dataplane: { attached: false } }),
    vm('d'),
  ];
  it('classifies health', () => {
    expect(netHealth(fleet[0])).toBe('ok');
    expect(netHealth(fleet[1])).toBe('warn');
    expect(netHealth(fleet[2])).toBe('warn');
    expect(netHealth(fleet[3])).toBe('idle');
  });
  it('rolls up counts', () => {
    const s = summarizeNetwork(fleet);
    expect(s).toMatchObject({ total: 4, attached: 2, policySynced: 1, antiSpoof: 1, needsAttention: 2 });
    expect(s.byPolicy).toEqual([{ policy: 'web', count: 2 }]);
  });
});
