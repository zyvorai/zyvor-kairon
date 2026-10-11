// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { ArrowDown, ArrowUp, Download, Search, X } from 'lucide-react';
import { ReactNode, useMemo, useState } from 'react';

export interface Column<T> {
  header: string;
  render: (item: T) => ReactNode;
  // Optional: makes the header click-to-sort. Return a string or number.
  sortValue?: (item: T) => string | number;
  // Optional plain-text form for CSV export; defaults to sortValue.
  csv?: (item: T) => string | number;
}

export function toCsv<T>(items: T[], columns: Column<T>[]): string {
  const cols = columns.filter((c) => c.csv || c.sortValue);
  const esc = (v: string | number) => {
    const s = String(v ?? '');
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  const rows = items.map((i) => cols.map((c) => esc((c.csv ?? c.sortValue!)(i))).join(','));
  return [cols.map((c) => esc(c.header)).join(','), ...rows].join('\n');
}

// filterRows keeps the items whose searchable text (every column's csv or
// sortValue, joined) contains every whitespace-separated word of query,
// case-insensitively. An empty query keeps everything.
export function filterRows<T>(items: T[], columns: Column<T>[], query: string): T[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return items;
  const cols = columns.filter((c) => c.csv || c.sortValue);
  return items.filter((item) => {
    const hay = cols.map((c) => String((c.csv ?? c.sortValue!)(item) ?? '')).join(' \n ').toLowerCase();
    return words.every((w) => hay.includes(w));
  });
}

// Shared list-table renderer: sortable headers, optional row selection
// (drives the Inspector) and CSV export. Pages that render extra controls
// per row (Machines, Migrations, Snapshots) pass their own action cell as
// a column.
export default function ResourceTable<T>({
  items,
  columns,
  keyFn,
  emptyText,
  onRowClick,
  selectedKey,
  csvName,
}: {
  items: T[];
  columns: Column<T>[];
  keyFn: (item: T) => string;
  emptyText: string;
  onRowClick?: (item: T) => void;
  selectedKey?: string;
  csvName?: string;
}) {
  const [sort, setSort] = useState<{ col: number; dir: 1 | -1 } | null>(null);
  const [query, setQuery] = useState('');
  const rows = useMemo(() => {
    const kept = filterRows(items, columns, query);
    if (!sort) return kept;
    const sv = columns[sort.col]?.sortValue;
    if (!sv) return kept;
    return [...kept].sort((a, b) => {
      const x = sv(a);
      const y = sv(b);
      return (typeof x === 'number' && typeof y === 'number' ? x - y : String(x).localeCompare(String(y))) * sort.dir;
    });
  }, [items, sort, columns, query]);

  function download() {
    const blob = new Blob([toCsv(rows, columns)], { type: 'text/csv' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = (csvName || 'kairon-export') + '.csv';
    a.click();
    URL.revokeObjectURL(a.href);
  }

  const canExport = !!csvName && items.length > 0;
  // A search box only earns its space once a list is long enough to need one.
  const canFilter = items.length > 5 || query !== '';
  return (
    <div className="table-wrap">
      {(canExport || canFilter) && (
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8, padding: '8px 10px 0' }}>
          {canFilter ? (
            <label style={{ display: 'inline-flex', alignItems: 'center', gap: 6, flex: '0 1 280px', minWidth: 0 }}>
              <Search size={13} aria-hidden />
              <input
                type="text"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder="Filter…"
                aria-label="Filter rows"
                style={{ flex: 1, minWidth: 0 }}
              />
              {query && (
                <button className="icon ghost" onClick={() => setQuery('')} aria-label="Clear filter">
                  <X size={13} />
                </button>
              )}
              {query && (
                <span className="msg" style={{ whiteSpace: 'nowrap' }}>
                  {rows.length} of {items.length}
                </span>
              )}
            </label>
          ) : (
            <span />
          )}
          {canExport && (
            <button className="sm ghost" onClick={download}>
              <Download size={13} /> CSV
            </button>
          )}
        </div>
      )}
      <table className="datatable">
        <thead>
          <tr>
            {columns.map((c, i) => (
              <th
                key={c.header}
                className={c.sortValue ? 'sortable' : undefined}
                aria-sort={sort?.col === i ? (sort.dir === 1 ? 'ascending' : 'descending') : undefined}
                onClick={c.sortValue ? () => setSort((s) => (s?.col === i ? { col: i, dir: (s.dir * -1) as 1 | -1 } : { col: i, dir: 1 })) : undefined}
              >
                {c.header}
                {sort?.col === i && (sort.dir === 1 ? <ArrowUp size={11} /> : <ArrowDown size={11} />)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((item) => {
            const k = keyFn(item);
            return (
              <tr
                key={k}
                className={(onRowClick ? 'clickable ' : '') + (selectedKey === k ? 'selected' : '')}
                onClick={onRowClick ? () => onRowClick(item) : undefined}
              >
                {columns.map((c) => (
                  <td key={c.header}>{c.render(item)}</td>
                ))}
              </tr>
            );
          })}
          {rows.length === 0 && (
            <tr>
              <td colSpan={columns.length} className="msg" style={{ textAlign: 'center', padding: 32 }}>
                {query && items.length > 0 ? `No rows match "${query}".` : emptyText}
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
