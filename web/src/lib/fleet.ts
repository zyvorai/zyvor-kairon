// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { relativeTime } from './timeline';

export interface FleetChild {
  name?: string;
  phase?: string;
}

export interface FleetStatus {
  phase?: string;
  message?: string;
  lastActionTime?: string;
  lastSampleTime?: string;
  desiredReplicas?: number;
  children?: FleetChild[];
  completed?: string[];
  totals?: Record<string, number>;
  watermark?: string;
  recoverySeconds?: number;
  dataAgeSeconds?: number;
}

export interface Fact {
  label: string;
  value: string;
}

// STALE_AFTER_SECONDS: telemetry older than this should not be trusted for
// a scaling or recovery decision, so the card flags it.
export const STALE_AFTER_SECONDS = 300;

export function isStale(s?: FleetStatus): boolean {
  return !!s && typeof s.dataAgeSeconds === 'number' && s.dataAgeSeconds > STALE_AFTER_SECONDS;
}

function num(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(2);
}

// factsOf lists the status fields worth a line on a card, in a stable order,
// skipping anything unset.
export function factsOf(s: FleetStatus | undefined, now: number = Date.now()): Fact[] {
  if (!s) return [];
  const out: Fact[] = [];
  if (typeof s.desiredReplicas === 'number' && s.desiredReplicas > 0) out.push({ label: 'Desired replicas', value: String(s.desiredReplicas) });
  if (s.children?.length) {
    const ready = s.children.filter((c) => c.phase === 'Running' || c.phase === 'Succeeded').length;
    out.push({ label: 'Children', value: `${ready}/${s.children.length} ready` });
  }
  if (s.completed?.length) out.push({ label: 'Completed steps', value: String(s.completed.length) });
  if (s.lastActionTime) out.push({ label: 'Last action', value: relativeTime(s.lastActionTime, now) || s.lastActionTime });
  if (s.lastSampleTime) out.push({ label: 'Last sample', value: relativeTime(s.lastSampleTime, now) || s.lastSampleTime });
  if (typeof s.dataAgeSeconds === 'number' && s.dataAgeSeconds > 0) out.push({ label: 'Data age', value: `${Math.round(s.dataAgeSeconds)} s` });
  if (typeof s.recoverySeconds === 'number' && s.recoverySeconds > 0) out.push({ label: 'Recovery time', value: `${num(s.recoverySeconds)} s` });
  for (const [k, v] of Object.entries(s.totals ?? {}).sort(([a], [b]) => a.localeCompare(b))) out.push({ label: k, value: num(v) });
  return out;
}
