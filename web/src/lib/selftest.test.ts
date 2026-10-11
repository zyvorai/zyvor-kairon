// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { formatAt, formatDuration, mergeLines, scenariosFor, stepGlyph, verdict, Line, Scenario } from './selftest';

const all: Scenario[] = [
  { id: 'control-plane', title: 'Control plane', profile: 'quick' },
  { id: 'boot', title: 'Boot', profile: 'quick' },
  { id: 'network', title: 'Network', profile: 'standard' },
  { id: 'fork', title: 'Fork', profile: 'full' },
];

describe('scenariosFor', () => {
  it('includes lower profiles', () => {
    expect(scenariosFor(all, 'quick', false).map((s) => s.id)).toEqual(['control-plane', 'boot']);
    expect(scenariosFor(all, 'standard', false).map((s) => s.id)).toEqual(['control-plane', 'boot', 'network']);
    expect(scenariosFor(all, 'full', false)).toHaveLength(4);
  });
  it('keeps only the control plane without a VM', () => {
    expect(scenariosFor(all, 'full', true).map((s) => s.id)).toEqual(['control-plane']);
  });
});

describe('formatting', () => {
  it('formats offsets and durations', () => {
    expect(formatAt(12400)).toBe('+12.4s');
    expect(formatAt(-5)).toBe('+0.0s');
    expect(formatDuration(0)).toBe('');
    expect(formatDuration(450)).toBe('450 ms');
    expect(formatDuration(12400)).toBe('12.4 s');
    expect(formatDuration(125000)).toBe('2m 5s');
  });
  it('maps step status to a glyph', () => {
    expect(stepGlyph('pass')).toBe('✓');
    expect(stepGlyph('fail')).toBe('✗');
    expect(stepGlyph('skip')).toBe('–');
    expect(stepGlyph('running')).toBe('▸');
  });
});

describe('verdict', () => {
  it('describes each state', () => {
    expect(verdict({ state: 'running' })).toBe('Running…');
    expect(verdict({ state: 'passed', summary: { Pass: 5, Fail: 0, Skip: 2 } })).toBe('Kairon is working · 5 passed · 0 failed · 2 skipped');
    expect(verdict({ state: 'failed', summary: { Pass: 1, Fail: 1, Skip: 0 } })).toContain('Some checks failed');
    expect(verdict({ state: 'passed', summary: { Pass: 0, Fail: 0, Skip: 3 } })).toContain('Nothing ran');
    expect(verdict({ state: 'cancelled' })).toBe('Cancelled');
  });
});

describe('mergeLines', () => {
  const l = (n: number): Line => ({ atMs: n, level: 'info', msg: String(n) });
  it('appends a fresh window', () => {
    expect(mergeLines([l(1), l(2)], [l(3)], 2).map((x) => x.msg)).toEqual(['1', '2', '3']);
  });
  it('ignores a duplicate window from a retried poll', () => {
    expect(mergeLines([l(1), l(2), l(3)], [l(2), l(3)], 1)).toHaveLength(3);
  });
  it('extends when the window overlaps', () => {
    expect(mergeLines([l(1), l(2)], [l(2), l(3)], 1).map((x) => x.msg)).toEqual(['1', '2', '3']);
  });
});
