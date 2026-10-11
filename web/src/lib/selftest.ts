// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

export type Level = 'info' | 'ok' | 'warn' | 'fail' | 'vm';
export type StepStatus = 'running' | 'pass' | 'fail' | 'skip';
export type RunState = 'running' | 'passed' | 'failed' | 'cancelled';

export interface Line {
  atMs: number;
  level: Level;
  step?: string;
  msg: string;
}

export interface Step {
  id: string;
  title: string;
  status: StepStatus;
  reason?: string;
  hint?: string;
  durationMs?: number;
}

export interface Run {
  id: string;
  profile: string;
  state: RunState;
  started: string;
  finished?: string;
  steps: Step[];
  lines: Line[];
  next: number;
  summary?: { Pass: number; Fail: number; Skip: number };
}

export interface Scenario {
  id: string;
  title: string;
  profile: 'quick' | 'standard' | 'full';
}

const RANK = { quick: 1, standard: 2, full: 3 } as const;

// scenariosFor lists what a profile will run, in catalogue order.
export function scenariosFor(all: Scenario[], profile: 'quick' | 'standard' | 'full', noVm: boolean): Scenario[] {
  return all.filter((s) => RANK[s.profile] <= RANK[profile] && (!noVm || s.id === 'control-plane'));
}

// formatAt renders a line's offset as "+12.4s".
export function formatAt(ms: number): string {
  return `+${(Math.max(0, ms) / 1000).toFixed(1)}s`;
}

export function formatDuration(ms?: number): string {
  if (ms === undefined || ms <= 0) return '';
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  return `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`;
}

export const GLYPH: Record<Level, string> = { info: '·', ok: '✓', warn: '!', fail: '✗', vm: 'vm│' };

export function stepGlyph(s: StepStatus): string {
  switch (s) {
    case 'pass':
      return '✓';
    case 'fail':
      return '✗';
    case 'skip':
      return '–';
    default:
      return '▸';
  }
}

// verdict is the one-line headline for a run.
export function verdict(run: Pick<Run, 'state' | 'summary'>): string {
  const s = run.summary;
  const counts = s ? `${s.Pass} passed · ${s.Fail} failed · ${s.Skip} skipped` : '';
  switch (run.state) {
    case 'running':
      return 'Running…';
    case 'cancelled':
      return counts ? `Cancelled · ${counts}` : 'Cancelled';
    case 'failed':
      return `Some checks failed · ${counts}`;
    default:
      return s && s.Pass === 0 ? `Nothing ran · ${counts}` : `Kairon is working · ${counts}`;
  }
}

// mergeLines appends newly polled lines, ignoring any the client already has
// (a retried poll can return an overlapping window).
export function mergeLines(have: Line[], incoming: Line[], after: number): Line[] {
  if (after >= have.length) return have.concat(incoming);
  if (after + incoming.length <= have.length) return have;
  return have.slice(0, after).concat(incoming);
}
