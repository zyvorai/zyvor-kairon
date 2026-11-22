// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText, getConfig } from '../api';
import { columnsOf, formatCell, Row, rowsOf } from '../lib/atlas';
import { EmptyState } from '../components/ui';
import JsonTerminal from '../components/JsonTerminal';
import ResourceTable from '../components/ResourceTable';
import Inspector, { KV } from '../components/Inspector';

const SECTIONS = [
  { id: 'volumes', label: 'Volumes', note: 'Owned by Kairon machines (owner kairon/machine/<ns>/<name>)' },
  { id: 'pools', label: 'Pools', note: '' },
  { id: 'alerts', label: 'Alerts', note: '' },
  { id: 'backups', label: 'Backups', note: '' },
  { id: 'snapshots', label: 'Snapshots', note: '' },
  { id: 'schedules', label: 'Schedules', note: '' },
  { id: 'jobs', label: 'Jobs', note: '' },
] as const;
type Section = (typeof SECTIONS)[number]['id'];

// Storage is the dashboard's read-only window onto Zyvor Atlas. All data comes
// through kairon-ui's allowlisted /api/v1/atlas proxy; the Atlas token never
// reaches the browser.
export default function Storage() {
  const [section, setSection] = useState<Section>('volumes');
  const [data, setData] = useState<unknown>();
  const [summary, setSummary] = useState<unknown>();
  const [err, setErr] = useState('');
  const [notConfigured, setNotConfigured] = useState(false);
  const [consoleURL, setConsoleURL] = useState('');
  const [selected, setSelected] = useState<Row | null>(null);

  useEffect(() => {
    getConfig()
      .then((c) => c.atlasConsoleURL && setConsoleURL(c.atlasConsoleURL))
      .catch(() => undefined);
    api('/api/v1/atlas/metrics/summary').then(setSummary).catch(() => undefined);
  }, []);

  useEffect(() => {
    setData(undefined);
    setErr('');
    setSelected(null);
    api(`/api/v1/atlas/${section}`)
      .then(setData)
      .catch((e) => {
        const t = apiErrorText(e);
        if (t.includes('not configured')) setNotConfigured(true);
        else setErr(t);
      });
  }, [section]);

  if (notConfigured) {
    return (
      <EmptyState title="Atlas is not connected">
        Set <code>KAIRON_UI_ATLAS_URL</code> (and <code>KAIRON_UI_ATLAS_TOKEN</code> or <code>KAIRON_UI_ATLAS_TOKEN_FILE</code>) on kairon-ui, or
        <code> ui.atlas.url</code> in the Helm chart, to see volumes, pools, alerts and backups here.
      </EmptyState>
    );
  }

  const rows = rowsOf(data);
  const cols = columnsOf(rows);
  const keyOf = (r: Row) => String(r.id ?? r.name ?? JSON.stringify(r).slice(0, 60));

  return (
    <div className="grid">
      <div className="card span4">
        <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, flexWrap: 'wrap' }}>
          <span className="eyebrow">STORAGE · ZYVOR ATLAS</span>
          {consoleURL && (
            <a href={consoleURL} target="_blank" rel="noopener noreferrer" className="linklike">
              Open in Atlas
            </a>
          )}
        </div>
        {summary !== undefined && <JsonTerminal title="cluster summary" value={summary} />}
        <div className="tabs" role="tablist">
          {SECTIONS.map((s) => (
            <button key={s.id} role="tab" aria-selected={section === s.id} className={section === s.id ? 'active' : ''} onClick={() => setSection(s.id)}>
              {s.label}
            </button>
          ))}
        </div>
        {err && <p className="msg error">{err}</p>}
        <ResourceTable
          items={rows}
          keyFn={keyOf}
          onRowClick={setSelected}
          selectedKey={selected ? keyOf(selected) : undefined}
          emptyText={data === undefined && !err ? 'Loading…' : `No ${section} to show.`}
          columns={cols.map((c) => ({ header: c, render: (r: Row) => formatCell(r[c]) }))}
        />
      </div>
      {selected && (
        <Inspector title={String(selected.name ?? selected.id ?? 'Details')} subtitle={String(selected.owner ?? section)} onClose={() => setSelected(null)}>
          <KV rows={Object.entries(selected).filter(([, v]) => v === null || typeof v !== 'object').map(([k, v]) => [k, formatCell(v)] as [string, string])} />
          <JsonTerminal title="raw" value={selected} />
        </Inspector>
      )}
    </div>
  );
}
