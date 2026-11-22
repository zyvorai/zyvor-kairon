// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useCallback, useEffect, useState } from 'react';
import { api, apiErrorText, apiJSON, isAdmin } from '../api';
import JsonTerminal, { Field } from './JsonTerminal';

type Tab = 'capabilities' | 'catalog' | 'pools' | 'templates' | 'sandboxes' | 'egress';
const TABS: { id: Tab; label: string }[] = [
  { id: 'capabilities', label: 'Capabilities' },
  { id: 'catalog', label: 'Image catalog' },
  { id: 'pools', label: 'Warm pools' },
  { id: 'templates', label: 'Templates' },
  { id: 'sandboxes', label: 'Sandboxes' },
  { id: 'egress', label: 'Egress check' },
];

type Item = Record<string, unknown>;

function itemsOf(v: unknown): Item[] {
  if (Array.isArray(v)) return v as Item[];
  if (v && typeof v === 'object') {
    const o = v as Record<string, unknown>;
    for (const k of ['items', 'images', 'pools', 'catalog']) if (Array.isArray(o[k])) return o[k] as Item[];
  }
  return [];
}
const nameOf = (i: Item) => String(i.name ?? i.id ?? i.tag ?? '');

// Per-node FluxVM administration surfaced from /api/v1/nodes/{node}/...
export default function NodeTools({ node }: { node: string }) {
  const base = `/api/v1/nodes/${encodeURIComponent(node)}`;
  const admin = isAdmin();
  const [tab, setTab] = useState<Tab>('capabilities');
  const [data, setData] = useState<unknown>();
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const [last, setLast] = useState<unknown>();
  const [form, setForm] = useState<Record<string, string>>({});
  const set = (k: string, v: string) => setForm((f) => ({ ...f, [k]: v }));

  const path = tab === 'egress' ? '' : tab;
  const load = useCallback(() => {
    if (!path) return;
    setData(undefined);
    setErr('');
    api(`${base}/${path}`).then(setData).catch((e) => setErr(apiErrorText(e)));
  }, [base, path]);
  useEffect(load, [load]);

  async function act(fn: () => Promise<unknown>, reload = true) {
    setBusy(true);
    setErr('');
    try {
      setLast((await fn()) ?? 'ok');
      if (reload) load();
    } catch (e) {
      setErr(apiErrorText(e));
    } finally {
      setBusy(false);
    }
  }
  const del = (p: string) => act(() => api(`${base}/${p}`, { method: 'DELETE' }));
  const post = (p: string, body: unknown = {}) => act(() => apiJSON(`${base}/${p}`, 'POST', body));
  const rows = itemsOf(data);

  return (
    <>
      <div className="tabs" role="tablist">
        {TABS.map((t) => (
          <button key={t.id} role="tab" aria-selected={tab === t.id} className={tab === t.id ? 'active' : ''} onClick={() => { setTab(t.id); setLast(undefined); }}>
            {t.label}
          </button>
        ))}
      </div>
      {err && <p className="msg error">{err}</p>}

      {tab === 'capabilities' && <JsonTerminal title="capabilities" value={data} />}

      {tab === 'catalog' && (
        <>
          <ul className="oplist">
            {rows.length === 0 && <li className="usageHint">No catalog images (or not loaded).</li>}
            {rows.map((i) => (
              <li key={nameOf(i)}>
                <span>
                  <b>{nameOf(i)}</b> <span className="usageHint">{String(i.format ?? '')} {i.readOnly ? 'read-only' : ''}</span>
                </span>
                {admin && (
                  <span className="rowactions">
                    <button className="sm" disabled={busy} onClick={() => post(`catalog/${encodeURIComponent(nameOf(i))}/read-only`, { readOnly: !i.readOnly })}>
                      {i.readOnly ? 'Make writable' : 'Make read-only'}
                    </button>
                    <button className="sm" disabled={busy} onClick={() => { const t = prompt('Clone to name:'); if (t) post(`catalog/${encodeURIComponent(nameOf(i))}/clone`, { targetName: t }); }}>Clone</button>
                    <button className="sm" disabled={busy} onClick={() => { const t = prompt('Rename to:'); if (t) post(`catalog/${encodeURIComponent(nameOf(i))}/rename`, { newName: t }); }}>Rename</button>
                    <button className="sm" disabled={busy} onClick={() => { const t = prompt('Export to node-local path:'); if (t) post(`catalog/${encodeURIComponent(nameOf(i))}/export`, { path: t }); }}>Export</button>
                    <button className="sm danger" disabled={busy} onClick={() => confirm(`Delete ${nameOf(i)} from the catalog?`) && del(`catalog/${encodeURIComponent(nameOf(i))}`)}>Delete</button>
                  </span>
                )}
              </li>
            ))}
          </ul>
          {admin && (
            <div className="opform">
              <Field label="Name"><input value={form.cname ?? ''} onChange={(e) => set('cname', e.target.value)} /></Field>
              <Field label="Source (path or URL)"><input value={form.csource ?? ''} onChange={(e) => set('csource', e.target.value)} /></Field>
              <Field label="Format"><input value={form.cformat ?? ''} onChange={(e) => set('cformat', e.target.value)} placeholder="qcow2" /></Field>
              <button disabled={busy || !form.cname || !form.csource} onClick={() => post('catalog', { name: form.cname, source: form.csource, format: form.cformat || undefined })}>
                {busy ? 'Adding…' : 'Add image'}
              </button>
              <button disabled={busy} onClick={() => confirm('Remove unreferenced catalog entries?') && post('catalog/clean')}>Clean</button>
            </div>
          )}
        </>
      )}

      {tab === 'pools' && (
        <>
          <ul className="oplist">
            {rows.length === 0 && <li className="usageHint">No warm pools.</li>}
            {rows.map((p) => (
              <li key={nameOf(p)}>
                <span>
                  <b>{nameOf(p)}</b> <span className="usageHint">size {String(p.size ?? '?')} · ready {String(p.ready ?? p.available ?? '?')}</span>
                </span>
                <span className="rowactions">
                  <button className="sm" disabled={busy} onClick={() => post(`pools/${encodeURIComponent(nameOf(p))}/claim`, {})}>Claim</button>
                  {admin && <button className="sm danger" disabled={busy} onClick={() => confirm(`Delete pool ${nameOf(p)} and ALL its VMs (including claimed)?`) && del(`pools/${encodeURIComponent(nameOf(p))}`)}>Delete</button>}
                </span>
              </li>
            ))}
          </ul>
          {admin && (
            <div className="opform">
              <Field label="Pool name"><input value={form.pname ?? ''} onChange={(e) => set('pname', e.target.value)} /></Field>
              <Field label="Size"><input type="number" value={form.psize ?? ''} onChange={(e) => set('psize', e.target.value)} /></Field>
              <Field label="Template (FluxVM CreateVm JSON)">
                <textarea rows={3} value={form.ptemplate ?? ''} onChange={(e) => set('ptemplate', e.target.value)} placeholder='{"name":"warm","cpu":1,"memory":"512Mi"}' />
              </Field>
              <button
                disabled={busy || !form.pname || !form.psize}
                onClick={() => {
                  try {
                    post('pools', { name: form.pname, size: Number(form.psize), template: JSON.parse(form.ptemplate || '{}') });
                  } catch {
                    setErr('Template must be valid JSON.');
                  }
                }}
              >
                Create pool
              </button>
            </div>
          )}
        </>
      )}

      {tab === 'templates' && (
        <>
          <ul className="oplist">
            {rows.length === 0 && <li className="usageHint">No templates.</li>}
            {rows.map((t) => (
              <li key={nameOf(t)}><b>{nameOf(t)}</b><span className="usageHint">{String(t.imageRef ?? '')}</span></li>
            ))}
          </ul>
          {admin && (
            <div className="opform">
              <Field label="Name"><input value={form.tname ?? ''} onChange={(e) => set('tname', e.target.value)} /></Field>
              <Field label="Image ref (OCI)"><input value={form.timage ?? ''} onChange={(e) => set('timage', e.target.value)} placeholder="registry/repo:tag" /></Field>
              <button disabled={busy || !form.tname || !form.timage} onClick={() => post('templates', { name: form.tname, imageRef: form.timage })}>
                {busy ? 'Building…' : 'Build template'}
              </button>
            </div>
          )}
        </>
      )}

      {tab === 'sandboxes' && <JsonTerminal title="sandboxes" value={data} />}

      {tab === 'egress' && (
        <div className="opform">
          {admin ? (
            <>
              <Field label="Host"><input value={form.host ?? ''} onChange={(e) => set('host', e.target.value)} placeholder="api.example.com" /></Field>
              <button disabled={busy || !form.host} onClick={() => act(() => apiJSON(`${base}/egress-check`, 'POST', { host: form.host }), false)}>Check</button>
            </>
          ) : (
            <p className="usageHint">Egress checks are admin-only.</p>
          )}
        </div>
      )}

      {last !== undefined && <JsonTerminal title="last result" value={last} />}
    </>
  );
}
