// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { Search } from 'lucide-react';
import { ReactNode, useEffect, useMemo, useRef, useState } from 'react';
import { fuzzyFilter } from '../lib/fuzzy';
import { NAV, NavItem, Page } from '../lib/nav';
import { iconFor } from './navIcons';
import type { Machine } from '../types';

export interface PaletteEntry {
  key: string;
  group: string;
  label: string;
  hint?: string;
  icon?: ReactNode;
  tone?: string;
  run: () => void;
}

// Builds the palette entries that are always available: every page.
export function pageEntries(go: (p: Page) => void): PaletteEntry[] {
  return NAV.flatMap((g) =>
    (g.items as readonly NavItem[]).map((i) => ({
      key: 'page:' + i.id,
      group: 'Go to',
      label: i.label,
      hint: g.group,
      icon: iconFor(i.id),
      tone: g.tone,
      run: () => go(i.id as Page),
    })),
  );
}

// Verb entries per machine ("Migrate web01", "Snapshot web01"). They only
// navigate and prefill the existing forms; nothing mutates from the palette,
// so a mistyped Enter cannot change the fleet.
export function machineVerbEntries(machines: Machine[], onMigrate: (name: string) => void, onSnapshot: (name: string) => void): PaletteEntry[] {
  return machines
    .filter((m) => m.status?.phase === 'Running')
    .flatMap<PaletteEntry>((m) => [
      { key: `verb:migrate:${m.metadata.namespace}/${m.metadata.name}`, group: 'Actions', label: `Migrate ${m.metadata.name}`, hint: 'Pick a target node', tone: 'emerald', run: () => onMigrate(m.metadata.name) },
      { key: `verb:snapshot:${m.metadata.namespace}/${m.metadata.name}`, group: 'Actions', label: `Snapshot ${m.metadata.name}`, hint: 'Point-in-time copy', tone: 'emerald', run: () => onSnapshot(m.metadata.name) },
    ]);
}

export default function CommandPalette({ entries, onClose }: { entries: PaletteEntry[]; onClose: () => void }) {
  const [q, setQ] = useState('');
  const [idx, setIdx] = useState(0);
  const list = useRef<HTMLDivElement>(null);
  const results = useMemo(() => fuzzyFilter(entries, q, (e) => `${e.label} ${e.hint ?? ''} ${e.group}`).slice(0, 40), [entries, q]);

  useEffect(() => setIdx(0), [q]);
  useEffect(() => {
    list.current?.querySelector('.palette-item.on')?.scrollIntoView({ block: 'nearest' });
  }, [idx]);

  function onKey(e: React.KeyboardEvent) {
    if (e.key === 'ArrowDown') (e.preventDefault(), setIdx((i) => Math.min(i + 1, results.length - 1)));
    else if (e.key === 'ArrowUp') (e.preventDefault(), setIdx((i) => Math.max(i - 1, 0)));
    else if (e.key === 'Enter') {
      e.preventDefault();
      const r = results[idx];
      if (r) (onClose(), r.run());
    } else if (e.key === 'Escape') onClose();
  }

  let lastGroup = '';
  return (
    <div className="palette-scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="Command palette" onKeyDown={onKey}>
        <input autoFocus value={q} onChange={(e) => setQ(e.target.value)} placeholder="Search pages, machines, actions…" aria-label="Search" />
        <div className="palette-list" ref={list}>
          {results.length === 0 && <div className="palette-empty">No matches for “{q}”.</div>}
          {results.map((r, i) => {
            const head = r.group !== lastGroup ? <div className="palette-group">{r.group}</div> : null;
            lastGroup = r.group;
            return (
              <div key={r.key}>
                {head}
                <button
                  className={'palette-item' + (i === idx ? ' on' : '')}
                  data-tone={r.tone}
                  onMouseEnter={() => setIdx(i)}
                  onClick={() => (onClose(), r.run())}
                >
                  {r.icon ?? <Search size={16} />}
                  <span>{r.label}</span>
                  {r.hint && <small>{r.hint}</small>}
                </button>
              </div>
            );
          })}
        </div>
        <div className="palette-foot">
          <span><kbd>↑</kbd> <kbd>↓</kbd> navigate</span>
          <span><kbd>↵</kbd> open</span>
          <span><kbd>esc</kbd> close</span>
        </div>
      </div>
    </div>
  );
}
