// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

export interface TimelineEntry {
  time: string;
  source: 'event' | 'condition';
  type: string;
  reason?: string;
  message?: string;
  status?: string;
  count?: number;
  origin?: string;
}

export type TimelineTone = 'ok' | 'warn' | 'error' | 'idle';

// Warning events and a False/Unknown condition read as attention; a True
// condition and Normal events read as healthy.
export function timelineTone(e: TimelineEntry): TimelineTone {
  if (e.source === 'event') return e.type === 'Warning' ? 'warn' : 'idle';
  if (e.status === 'True') return 'ok';
  if (e.status === 'False') return 'error';
  return 'warn';
}

// relativeTime renders "just now", "5m ago", "3h ago", "2d ago". An
// unparsable or zero Go time ("0001-01-01...") renders as an empty string.
export function relativeTime(iso: string, now: number = Date.now()): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t) || t <= 0 || new Date(t).getUTCFullYear() < 1971) return '';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${Math.round(s / 3600)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}
