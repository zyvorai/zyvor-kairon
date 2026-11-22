// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest';
import { fuzzyFilter, fuzzyScore } from './fuzzy';
import { groupOf, isPage, labelOf, NAV, PAGES } from './nav';
import { formatHash, parseHash } from './route';
import { resolveTheme } from './theme';

describe('nav', () => {
  it('has unique ids and every page resolves to a group (except account)', () => {
    expect(new Set(PAGES).size).toBe(PAGES.length);
    for (const p of PAGES) {
      if (p === 'account') continue;
      expect(groupOf(p)).toBeDefined();
    }
    expect(NAV.length).toBeGreaterThan(3);
  });
  it('labels and validates pages', () => {
    expect(labelOf('machinesets')).toBe('Machine sets');
    expect(labelOf('account')).toBe('Account');
    expect(isPage('machines')).toBe(true);
    expect(isPage('nope')).toBe(false);
  });
});

describe('route', () => {
  it('parses page and target', () => {
    expect(parseHash('#/machines')).toEqual({ page: 'machines', target: undefined });
    expect(parseHash('#/machines/default/web%2001')).toEqual({ page: 'machines', target: 'default/web 01' });
  });
  it('falls back to overview', () => {
    expect(parseHash('')).toEqual({ page: 'overview' });
    expect(parseHash('#/bogus/x')).toEqual({ page: 'overview' });
  });
  it('round-trips', () => {
    const r = { page: 'machines' as const, target: 'default/web 01' };
    expect(parseHash(formatHash(r))).toEqual(r);
  });
});

describe('fuzzy', () => {
  it('matches subsequences and rejects non-matches', () => {
    expect(fuzzyScore('mach', 'Machines')).not.toBeNull();
    expect(fuzzyScore('xz', 'Machines')).toBeNull();
    expect(fuzzyScore('', 'anything')).toBe(0);
  });
  it('ranks tighter matches first', () => {
    const out = fuzzyFilter(['Snapshot schedules', 'Snapshots', 'Restores'], 'snap', (s) => s);
    expect(out[0]).toBe('Snapshots');
    expect(out).not.toContain('Restores');
  });
});

describe('theme', () => {
  it('resolves system by preference', () => {
    expect(resolveTheme('system', true)).toBe('dark');
    expect(resolveTheme('system', false)).toBe('light');
    expect(resolveTheme('light', true)).toBe('light');
  });
});
