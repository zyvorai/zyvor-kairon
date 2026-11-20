// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest';
import { columnsOf, formatCell, rowsOf } from './atlas';

describe('atlas helpers', () => {
  it('finds rows in arrays and wrapped objects', () => {
    expect(rowsOf([{ id: 'a' }, 3])).toHaveLength(1);
    expect(rowsOf({ items: [{ id: 'a' }, { id: 'b' }] })).toHaveLength(2);
    expect(rowsOf({ volumes: [{ id: 'v' }] })).toHaveLength(1);
    expect(rowsOf('nope')).toHaveLength(0);
  });
  it('prefers identifying columns and skips objects', () => {
    const cols = columnsOf([{ zzz: 1, owner: 'o', id: 'i', nested: { a: 1 } }]);
    expect(cols.slice(0, 2)).toEqual(['id', 'owner']);
    expect(cols).not.toContain('nested');
  });
  it('humanizes large byte counts', () => {
    expect(formatCell(5 * 1024 * 1024 * 1024)).toBe('5.0 GiB');
    expect(formatCell('x')).toBe('x');
  });
});
