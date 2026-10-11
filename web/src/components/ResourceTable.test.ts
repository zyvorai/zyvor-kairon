// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { filterRows, toCsv } from './ResourceTable';

describe('toCsv', () => {
  it('exports sortable/csv columns and escapes', () => {
    const items = [{ n: 'a,b', c: 2 }, { n: 'say "hi"', c: 3 }];
    const out = toCsv(items, [
      { header: 'Name', render: (i) => i.n, sortValue: (i) => i.n },
      { header: 'CPU', render: (i) => i.c, csv: (i) => i.c },
      { header: 'Actions', render: () => null },
    ]);
    expect(out).toBe('Name,CPU\n"a,b",2\n"say ""hi""",3');
  });
});

describe('filterRows', () => {
  const items = [
    { name: 'web-1', node: 'worker-1', phase: 'Running' },
    { name: 'web-2', node: 'worker-2', phase: 'Stopped' },
    { name: 'db-1', node: 'worker-1', phase: 'Running' },
  ];
  const cols = [
    { header: 'Name', render: (i: (typeof items)[number]) => i.name, sortValue: (i: (typeof items)[number]) => i.name },
    { header: 'Node', render: (i: (typeof items)[number]) => i.node, sortValue: (i: (typeof items)[number]) => i.node },
    { header: 'Phase', render: (i: (typeof items)[number]) => i.phase, sortValue: (i: (typeof items)[number]) => i.phase },
    { header: 'Actions', render: () => null }, // not searchable: no sortValue/csv
  ];

  it('keeps everything for an empty or blank query', () => {
    expect(filterRows(items, cols, '')).toEqual(items);
    expect(filterRows(items, cols, '   ')).toEqual(items);
  });

  it('matches case-insensitively across columns', () => {
    expect(filterRows(items, cols, 'WEB').map((i) => i.name)).toEqual(['web-1', 'web-2']);
    expect(filterRows(items, cols, 'stopped').map((i) => i.name)).toEqual(['web-2']);
  });

  it('requires every word to match', () => {
    expect(filterRows(items, cols, 'worker-1 running').map((i) => i.name)).toEqual(['web-1', 'db-1']);
    expect(filterRows(items, cols, 'web running').map((i) => i.name)).toEqual(['web-1']);
    expect(filterRows(items, cols, 'nothing')).toEqual([]);
  });

  it('does not search columns that have no text form', () => {
    expect(filterRows(items, cols, 'actions')).toEqual([]);
  });
});
