// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

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
  withBlackhole: number;
  maxBlackholeMs: number;
  byPolicy: { policy: string; count: number }[];
}

export function summarizeNetwork(machines: Machine[]): NetSummary {
  const s: NetSummary = { total: machines.length, attached: 0, policySynced: 0, antiSpoof: 0, needsAttention: 0, withBlackhole: 0, maxBlackholeMs: 0, byPolicy: [] };
  const policies = new Map<string, number>();
  for (const m of machines) {
    const n = m.status?.network;
    if (n?.dataplane?.attached) s.attached++;
    if (n?.dataplane?.policySynced) s.policySynced++;
    if (n?.edge?.antiSpoof) s.antiSpoof++;
    if (netHealth(m) === 'warn') s.needsAttention++;
    const bh = n?.edge?.blackholeWindowMs ?? 0;
    if (bh > 0) {
      s.withBlackhole++;
      if (bh > s.maxBlackholeMs) s.maxBlackholeMs = bh;
    }
    const p = n?.edge?.policyName;
    if (p) policies.set(p, (policies.get(p) ?? 0) + 1);
  }
  s.byPolicy = [...policies].map(([policy, count]) => ({ policy, count })).sort((a, b) => b.count - a.count || a.policy.localeCompare(b.policy));
  return s;
}

export interface ProcessAttr {
  source?: string;
  scope?: string;
  pid?: number;
  tid?: number;
  uid?: number;
  comm?: string;
  executable?: string;
  cgroup?: string;
  cgroupID?: number;
  container?: string;
}

export interface LatencyAttr {
  rttUs?: number;
  rttVarUs?: number;
  minRttUs?: number;
  maxRttUs?: number;
  samples?: number;
}

export interface FlowRow {
  proto: string;
  src: string;
  dst: string;
  packets: number;
  bytes: number;
  reason: string;
  process: string;
  hostProcess: string;
  rtt: string;
}

export interface DropRow {
  reason: string;
  policy: string;
  src: string;
  dst: string;
  packets: number;
  process: string;
  rtt: string;
}

export interface StatsSummary {
  rxPackets: number;
  txPackets: number;
  rxBytes: number;
  txBytes: number;
  dropped: number;
  flows?: number;
  rtt: string;
  minRtt: string;
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function num(v: unknown): number {
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v === 'string' && v !== '') {
    const n = Number(v);
    if (Number.isFinite(n)) return n;
  }
  return 0;
}

function str(...vals: unknown[]): string {
  for (const v of vals) {
    if (typeof v === 'string' && v.trim() && v.trim() !== '-') return v.trim();
  }
  return '';
}

function pick(obj: Record<string, unknown> | undefined, ...keys: string[]): unknown {
  if (!obj) return undefined;
  for (const k of keys) {
    if (obj[k] !== undefined && obj[k] !== null) return obj[k];
  }
  return undefined;
}

function endpoint(ip: string, port: number): string {
  if (!ip) return '-';
  return port ? `${ip}:${port}` : ip;
}

export function formatMicros(us: number): string {
  if (!us) return '-';
  if (us < 1000) return `${us}µs`;
  if (us < 1_000_000) return us % 1000 === 0 ? `${us / 1000}ms` : `${(us / 1000).toFixed(1)}ms`;
  return us % 1_000_000 === 0 ? `${us / 1_000_000}s` : `${(us / 1_000_000).toFixed(2)}s`;
}

function latencyOf(obj: Record<string, unknown> | undefined): LatencyAttr {
  const nested = asRecord(obj?.latency);
  const src = { ...(obj ?? {}), ...(nested ?? {}) };
  let rttUs = num(pick(src, 'rttUs', 'rtt_us', 'srttUs', 'srtt_us'));
  if (!rttUs) {
    const ms = num(pick(src, 'rttMs', 'rtt_ms'));
    if (ms) rttUs = Math.round(ms * 1000);
  }
  return {
    rttUs,
    rttVarUs: num(pick(src, 'rttVarUs', 'rtt_var_us')),
    minRttUs: num(pick(src, 'minRttUs', 'min_rtt_us')),
    maxRttUs: num(pick(src, 'maxRttUs', 'max_rtt_us')),
    samples: num(pick(src, 'samples')),
  };
}

function processOf(obj: Record<string, unknown> | undefined, nestedKey = 'process'): ProcessAttr {
  const nested = asRecord(obj?.[nestedKey]);
  const src = nested ?? obj ?? {};
  return {
    source: str(src.source),
    scope: str(src.scope),
    pid: num(pick(src, 'pid', 'process_id', 'processId')) || undefined,
    tid: num(pick(src, 'tid', 'thread_id')) || undefined,
    comm: str(src.comm, src.comm_name, src.processName),
    executable: str(src.executable, src.exe, src.binary),
    cgroup: str(src.cgroup, src.cgroup_path, src.cgroupPath),
    cgroupID: num(pick(src, 'cgroupID', 'cgroup_id', 'cgroupId')) || undefined,
    container: str(src.container, src.container_id, src.containerId),
  };
}

export function formatProcess(p: ProcessAttr | undefined): string {
  if (!p) return '-';
  const name = p.comm || p.executable || p.container || '';
  const extra: string[] = [];
  if (p.pid) extra.push(`pid=${p.pid}`);
  if (p.cgroup) extra.push(`cgroup=${p.cgroup.length > 40 ? `…${p.cgroup.slice(-39)}` : p.cgroup}`);
  else if (p.cgroupID) extra.push(`cgroup=${p.cgroupID}`);
  if (!name && !extra.length && !p.scope) return '-';
  const head = `${p.scope ? `${p.scope}:` : ''}${name}`;
  if (!extra.length) return head || '-';
  return name ? `${head}[${extra.join(',')}]` : `${head}${extra.join(',')}`;
}


export type ProcessBadgeKind = 'guest' | 'host' | 'mixed';

export interface ProcessBadge {
  label: string;
  kind: ProcessBadgeKind;
}

// processBadge is the Network panel header chip. It is hidden when no
// flow or drop carries process identity (Kairon never invents one).
export function processBadge(rows: Array<{ process?: string; hostProcess?: string }>): ProcessBadge | undefined {
  const labels: string[] = [];
  const scopes = new Set<string>();
  for (const row of rows) {
    for (const raw of [row.process, row.hostProcess]) {
      if (!raw || raw === '-') continue;
      labels.push(raw);
      const m = /^(guest|host):/.exec(raw);
      if (m) scopes.add(m[1]);
    }
  }
  if (!labels.length) return undefined;
  const unique = [...new Set(labels)];
  let kind: ProcessBadgeKind = 'guest';
  if (scopes.has('guest') && scopes.has('host')) kind = 'mixed';
  else if (scopes.has('host') && !scopes.has('guest')) kind = 'host';
  const label = unique.length === 1 ? unique[0] : `${unique[0]} +${unique.length - 1}`;
  return { label, kind };
}

function unwrapList(data: unknown, keys: string[]): Record<string, unknown>[] | undefined {
  if (Array.isArray(data)) return data.map(asRecord).filter((x): x is Record<string, unknown> => !!x);
  const obj = asRecord(data);
  if (!obj) return undefined;
  for (const k of keys) {
    const v = obj[k];
    if (Array.isArray(v)) return v.map(asRecord).filter((x): x is Record<string, unknown> => !!x);
  }
  // A single flow/drop object.
  if (str(obj.proto, obj.protocol, obj.reason, obj.srcIP, obj.src_ip) || num(obj.packets) || num(obj.count)) {
    return [obj];
  }
  return undefined;
}

export function parseFlows(data: unknown): FlowRow[] | undefined {
  const items = unwrapList(data, ['flows', 'items', 'data']);
  if (!items) return undefined;
  return items.map((o) => {
    const proc = processOf(o);
    const lat = latencyOf(o);
    const proto = str(o.proto, o.protocol);
    const srcIP = str(o.srcIP, o.src_ip, o.src);
    const dstIP = str(o.dstIP, o.dst_ip, o.dst);
    return {
      proto: proto || '-',
      src: endpoint(srcIP, num(pick(o, 'srcPort', 'src_port'))),
      dst: endpoint(dstIP, num(pick(o, 'dstPort', 'dst_port'))),
      packets: num(pick(o, 'packets', 'packet_count', 'count')),
      bytes: num(pick(o, 'bytes', 'byte_count')),
      reason: str(o.reason, o.dropReason) || '-',
      process: formatProcess(proc),
      hostProcess: formatProcess(processOf(o, asRecord(o.hostProcess) ? 'hostProcess' : 'host_process')),
      rtt: formatMicros(lat.rttUs ?? 0),
    };
  });
}

export function parseDrops(data: unknown): DropRow[] | undefined {
  const items = unwrapList(data, ['drops', 'items', 'data', 'events']);
  if (!items) return undefined;
  return items.map((o) => {
    const proc = processOf(o);
    const host = processOf(o, 'hostProcess');
    const lat = latencyOf(o);
    let process = formatProcess(proc);
    if (process === '-') process = formatProcess(host);
    return {
      reason: str(o.reason, o.dropReason, o.drop_reason) || '-',
      policy: str(o.policyName, o.policy, o.policy_name) || '-',
      src: endpoint(str(o.srcIP, o.src_ip, o.src), num(pick(o, 'srcPort', 'src_port'))),
      dst: endpoint(str(o.dstIP, o.dst_ip, o.dst), num(pick(o, 'dstPort', 'dst_port'))),
      packets: num(pick(o, 'packets', 'packet_count', 'count')),
      process,
      rtt: formatMicros(lat.rttUs ?? 0),
    };
  });
}

export function parseStats(data: unknown): StatsSummary | undefined {
  const o = asRecord(data);
  if (!o) return undefined;
  const lat = latencyOf(o);
  return {
    rxPackets: num(pick(o, 'rxPackets', 'rx_packets', 'rxPkts')),
    txPackets: num(pick(o, 'txPackets', 'tx_packets', 'txPkts')),
    rxBytes: num(pick(o, 'rxBytes', 'rx_bytes')),
    txBytes: num(pick(o, 'txBytes', 'tx_bytes')),
    dropped: num(pick(o, 'dropped', 'drops', 'dropPackets')),
    flows: num(pick(o, 'flows', 'flowCount', 'flow_count')) || undefined,
    rtt: formatMicros(lat.rttUs ?? 0),
    minRtt: formatMicros(lat.minRttUs ?? 0),
  };
}
