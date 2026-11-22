// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Badge color classification and formatting helpers shared by the
// Machines/Migrations/Snapshots pages. Phase strings are exactly what
// internal/agent and internal/controller set on status.phase -- see
// normalizePhase (Machine) and the migration state machine documented in
// docs/architecture.md (MachineMigration): Pending -> {Starting|Stopping}
// -> ... -> {Cutover->Adopting->Succeeded | Stopping->Restarting->Succeeded}
// | Blocked | Failed | NeedsRecovery | Cancelled (operator-requested, via
// spec.cancel, only reachable from Starting/Running).

export function badgeClass(phase: string): string {
  switch (phase) {
    case 'Running':
    case 'Succeeded':
    case 'True':
      return 'badge badge-ok';
    case 'Starting':
    case 'Cutover':
    case 'Adopting':
    case 'Stopping':
    case 'Restarting':
    case 'Pending':
      return 'badge badge-progress';
    case 'NeedsRecovery':
    case 'Paused':
      return 'badge badge-warn';
    case 'Failed':
    case 'Blocked':
    case 'Error':
      return 'badge badge-error';
    case 'Stopped':
    case 'Halted':
    case 'Cancelled':
      return 'badge badge-idle';
    default:
      return 'badge badge-idle';
  }
}

// formatBytes renders a byte count (Kairon reports RAM in bytes, e.g.
// status.ramTransferred/ramTotal) as a short human-readable string.
export function formatBytes(n?: number): string {
  if (!n || n <= 0) return '-';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  const decimals = i === 0 || v >= 10 || v % 1 === 0 ? 0 : 1;
  return `${v.toFixed(decimals)} ${units[i]}`;
}

// formatCPUPercent mirrors kaironctl's own formatCPUPercent
// (internal/kaironctl/kaironctl.go): a Machine/Node's CPUPercent is
// already a percentage of one core (can exceed 100 for a multi-vCPU
// Machine or a hot node using more than one core's worth of time), not a
// millicore count, so this is simply a fixed, readable precision --
// matching `kaironctl top`'s own two-decimal rendering exactly, rather
// than a kubectl-top-style "123m" notation that doesn't apply here.
export function formatCPUPercent(p: number): string {
  return `${p.toFixed(2)}%`;
}

// formatDuration renders a millisecond count (status.totalTimeMs/downtimeMs)
// as a short human-readable string.
export function formatDuration(ms?: number): string {
  if (!ms || ms <= 0) return '-';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  return `${m}m${rem}s`;
}

export function transferProgress(transferred?: number, total?: number): number | null {
  if (!total || total <= 0) return null;
  const pct = ((transferred ?? 0) / total) * 100;
  return Math.max(0, Math.min(100, Math.round(pct)));
}

// nodeReadyStatus/nodeTaintsSummary reimplement kaironctl's own
// nodeReadyStatus/nodeTaintsSummary (internal/kaironctl/kaironctl.go) for
// the dashboard's Nodes page (web/src/pages/Nodes.tsx) -- same fields, same
// "Unknown"/"-" fallbacks, so an operator sees the identical answer to "is
// this node Ready" and "what's tainted" whether they ask kaironctl or the
// dashboard. Kept as a from-scratch TypeScript reimplementation rather than
// a shared artifact, the same tradeoff kaironctl's own Go doc comment
// already made for reusing internal/scheduler's unexported taintKV: two
// small, purely cosmetic formatting helpers with no shared behavior that
// could drift out of sync.
export function nodeReadyStatus(node: { status?: { conditions?: { type: string; status: string }[] } }): string {
  const cond = (node.status?.conditions || []).find((c) => c.type === 'Ready');
  return cond ? cond.status : 'Unknown';
}

export function nodeTaintsSummary(node: { spec?: { taints?: { key: string; value?: string; effect: string }[] } }): string {
  const taints = node.spec?.taints || [];
  if (taints.length === 0) return '-';
  return taints.map((t) => (t.value ? `${t.key}=${t.value}` : t.key) + ':' + t.effect).join(', ');
}
