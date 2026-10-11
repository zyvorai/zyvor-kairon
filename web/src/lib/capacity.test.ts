// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { barTone, formatCores, formatMiB, pct } from './capacity';

describe('capacity helpers', () => {
  it('computes percentages and guards zero', () => {
    expect(pct(3000, 4000)).toBe(75);
    expect(pct(5000, 4000)).toBe(125);
    expect(pct(1, 0)).toBeNull();
  });
  it('picks a tone by fullness', () => {
    expect(barTone(null)).toBe('idle');
    expect(barTone(50)).toBe('ok');
    expect(barTone(80)).toBe('warn');
    expect(barTone(95)).toBe('error');
  });
  it('formats cores and memory', () => {
    expect(formatCores(4000)).toBe('4');
    expect(formatCores(2500)).toBe('2.5');
    expect(formatMiB(512)).toBe('512 MiB');
    expect(formatMiB(8192)).toBe('8 GiB');
    expect(formatMiB(1536)).toBe('1.5 GiB');
  });
});
