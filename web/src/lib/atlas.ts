// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Helpers for rendering Atlas gateway payloads whose exact row shape varies
// by resource: a bare array, or an object holding the array under a known key.

export type Row = Record<string, unknown>;

export function rowsOf(v: unknown): Row[] {
  if (Array.isArray(v)) return v.filter((x): x is Row => !!x && typeof x === 'object');
  if (v && typeof v === 'object') {
    const o = v as Record<string, unknown>;
    for (const k of ['items', 'volumes', 'pools', 'osds', 'alerts', 'backups', 'snapshots', 'schedules', 'jobs']) {
      if (Array.isArray(o[k])) return rowsOf(o[k]);
    }
  }
  return [];
}

// columnsOf picks up to `max` scalar keys, preferring identifying ones.
export function columnsOf(rows: Row[], max = 6): string[] {
  const prefer = ['id', 'name', 'owner', 'state', 'status', 'phase', 'severity', 'size', 'sizeBytes', 'used', 'pool', 'kind', 'type', 'createdAt'];
  const seen = new Set<string>();
  for (const r of rows.slice(0, 20)) {
    for (const [k, v] of Object.entries(r)) {
      if (v === null || typeof v === 'object') continue;
      seen.add(k);
    }
  }
  const ordered = [...prefer.filter((k) => seen.has(k)), ...[...seen].filter((k) => !prefer.includes(k))];
  return ordered.slice(0, max);
}

export function formatCell(v: unknown): string {
  if (v === undefined || v === null) return '';
  if (typeof v === 'number' && v > 1024 * 1024) {
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let n = v;
    let i = 0;
    while (n >= 1024 && i < units.length - 1) {
      n /= 1024;
      i++;
    }
    return `${n.toFixed(1)} ${units[i]}`;
  }
  return String(v);
}
