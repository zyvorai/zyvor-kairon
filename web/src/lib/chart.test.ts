// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { linePath } from './chart';

describe('linePath', () => {
  it('is empty for no points', () => {
    expect(linePath([], 100, 20)).toBe('');
  });
  it('draws low values at the bottom and high at the top', () => {
    const d = linePath([{ t: 0, v: 0 }, { t: 10, v: 10 }], 100, 20);
    expect(d).toBe('M2.0,18.0 L98.0,2.0');
  });
  it('draws a flat line mid-height', () => {
    const d = linePath([{ t: 0, v: 5 }, { t: 10, v: 5 }], 100, 20);
    expect(d).toBe('M2.0,10.0 L98.0,10.0');
  });
  it('handles a single point', () => {
    expect(linePath([{ t: 5, v: 1 }], 100, 20)).toBe('M2.0,10.0 L98.0,10.0');
  });
});
