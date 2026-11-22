// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest';
import { countUpValue, easeOutCubic } from './countUp';

describe('countUp', () => {
  it('eases between 0 and 1', () => {
    expect(easeOutCubic(0)).toBe(0);
    expect(easeOutCubic(1)).toBe(1);
    expect(easeOutCubic(2)).toBe(1);
    expect(easeOutCubic(0.5)).toBeGreaterThan(0.5);
  });
  it('lands exactly on the target', () => {
    expect(countUpValue(0, 40, 0, 600)).toBe(0);
    expect(countUpValue(0, 40, 600, 600)).toBe(40);
    expect(countUpValue(10, 20, 100, 0)).toBe(20);
  });
});
