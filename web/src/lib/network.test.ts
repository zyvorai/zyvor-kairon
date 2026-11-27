// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, it, expect } from 'vitest';
import { netHealth, summarizeNetwork, parseFlows, parseDrops, parseStats, formatMicros, formatProcess, processBadge } from './network';
import { Machine } from '../types';

function vm(name: string, network?: NonNullable<Machine['status']>['network']): Machine {
  return { metadata: { name }, spec: { image: { path: '' }, resources: { cpu: '1', memory: '1Gi' } }, status: { network } };
}

describe('network summary', () => {
  const fleet = [
    vm('a', { dataplane: { attached: true, policySynced: true }, edge: { antiSpoof: true, policyName: 'web', blackholeWindowMs: 40 } }),
    vm('b', { dataplane: { attached: true, policySynced: false }, edge: { policyName: 'web', blackholeWindowMs: 12 } }),
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
    expect(s).toMatchObject({ total: 4, attached: 2, policySynced: 1, antiSpoof: 1, needsAttention: 2, withBlackhole: 2, maxBlackholeMs: 40 });
    expect(s.byPolicy).toEqual([{ policy: 'web', count: 2 }]);
  });
});

describe('flow and drop parsers', () => {
  it('formats latency', () => {
    expect(formatMicros(0)).toBe('-');
    expect(formatMicros(800)).toBe('800µs');
    expect(formatMicros(1400)).toBe('1.4ms');
    expect(formatMicros(1000)).toBe('1ms');
  });
  it('formats process identity', () => {
    expect(formatProcess({})).toBe('-');
    expect(formatProcess({ scope: 'guest', comm: 'nginx', pid: 142 })).toBe('guest:nginx[pid=142]');
  });
  it('parses wrapped snake_case flows with process and rtt', () => {
    const rows = parseFlows({ flows: [{ proto: 'tcp', src_ip: '10.0.0.8', dst_ip: '1.1.1.1', src_port: 40000, dst_port: 443, packets: 12, bytes: 8400, comm: 'nginx', pid: 142, scope: 'guest', rtt_us: 1400 }] });
    expect(rows).toHaveLength(1);
    expect(rows![0]).toMatchObject({ proto: 'tcp', src: '10.0.0.8:40000', dst: '1.1.1.1:443', packets: 12, rtt: '1.4ms', process: 'guest:nginx[pid=142]' });
  });
  it('parses attributed drops', () => {
    const rows = parseDrops({ drops: [{ reason: 'sni_deny', policy: 'egress-443', src_ip: '10.0.0.8', dst_ip: '9.9.9.9', dst_port: 443, count: 4, process: { comm: 'curl', pid: 77, scope: 'guest' } }] });
    expect(rows![0]).toMatchObject({ reason: 'sni_deny', policy: 'egress-443', packets: 4, process: 'guest:curl[pid=77]' });
  });
  it('returns undefined for an unrelated object', () => {
    expect(parseFlows({ message: 'dataplane not attached' })).toBeUndefined();
  });
  it('parses dataplane stats including rtt', () => {
    const s = parseStats({ rx_packets: 10, txPackets: 12, dropped: 3, rttMs: 2.5, min_rtt_us: 800 });
    expect(s).toMatchObject({ rxPackets: 10, txPackets: 12, dropped: 3, rtt: '2.5ms', minRtt: '800µs' });
  });
  it('builds a process badge only when attribution is present', () => {
    expect(processBadge([])).toBeUndefined();
    expect(processBadge([{ process: '-', hostProcess: '-' }])).toBeUndefined();
    const one = processBadge(parseFlows({ flows: [{ proto: 'tcp', comm: 'nginx', pid: 142, scope: 'guest' }] })!);
    expect(one).toEqual({ label: 'guest:nginx[pid=142]', kind: 'guest' });
    const mixed = processBadge([
      { process: 'guest:nginx[pid=142]', hostProcess: '-' },
      { process: '-', hostProcess: 'host:vhost[pid=9]' },
    ]);
    expect(mixed).toEqual({ label: 'guest:nginx[pid=142] +1', kind: 'mixed' });
  });
});
