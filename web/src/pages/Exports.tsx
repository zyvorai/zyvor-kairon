// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineExport } from '../types';
import ResourceTable from '../components/ResourceTable';
import { badgeClass } from '../lib/phase';
import { currentNamespace } from '../lib/namespace';
import EmptyHint from '../components/EmptyHint';

function formatBytes(n?: number): string {
  if (!n) return '-';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function Exports() {
  const [items, setItems] = useState<MachineExport[]>([]);
  const [machineName, setMachineName] = useState('');
  const [format, setFormat] = useState('');
  const [ttl, setTtl] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);

  const refresh = () =>
    api<MachineExport[]>('/api/v1/exports').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    if (!machineName) return;
    setBusy(true);
    setMsg('');
    try {
      await apiJSON('/api/v1/exports', 'POST', { machineName, format, ttl });
      setMachineName('');
      setFormat('');
      setTtl('');
      await refresh();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  const download = async (name: string) => {
    try {
      const r = await apiJSON<{ url: string }>(`/api/v1/exports/${currentNamespace()}/${encodeURIComponent(name)}/link`, 'POST', {});
      window.location.assign(r.url);
    } catch (e) {
      setMsg(String(e));
    }
  };

  const remove = async (name: string) => {
    if (!confirm(`Delete MachineExport "${name}"? The node stops serving the disk.`)) return;
    try {
      await api(`/api/v1/exports/${currentNamespace()}/${encodeURIComponent(name)}`, { method: 'DELETE' });
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">CREATE EXPORT</span>
        <h3>Download a machine's boot disk</h3>
        <p className="muted">The machine must be Stopped or Halted. A Stopped machine needs a PersistentVolume boot disk.</p>
        <form onSubmit={create}>
          <div className="formgrid">
            <label>
              Machine
              <input required value={machineName} onChange={(e) => setMachineName(e.target.value)} />
            </label>
            <label>
              Format
              <select value={format} onChange={(e) => setFormat(e.target.value)}>
                <option value="">disk's own format</option>
                <option value="qcow2">qcow2</option>
                <option value="raw">raw</option>
              </select>
            </label>
            <label>
              TTL
              <input value={ttl} onChange={(e) => setTtl(e.target.value)} placeholder="2h if empty, at most 168h" />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Creating...' : 'Create export'}
            </button>
            {msg && <span className={msg.toLowerCase().includes('error') ? 'msg error' : 'msg'}>{msg}</span>}
          </div>
        </form>
      </div>

      <div className="card span4">
        <span className="eyebrow">EXPORTS</span>
        <ResourceTable
          items={items}
          keyFn={(e) => e.metadata.name}
          emptyText={<EmptyHint what="exports" hint="Create one above, or: kaironctl export machine NAME -o disk.qcow2." />}
          columns={[
            { header: 'Name', render: (e) => e.metadata.name },
            { header: 'Machine', render: (e) => e.spec.machineName },
            { header: 'Format', render: (e) => e.status?.format || e.spec.format || '-' },
            {
              header: 'Phase',
              render: (e) => <span className={badgeClass(e.status?.phase || '')}>{e.status?.phase || 'Pending'}</span>,
            },
            { header: 'Size', render: (e) => formatBytes(e.status?.sizeBytes) },
            { header: 'Expires', render: (e) => (e.status?.expiresAt ? new Date(e.status.expiresAt).toLocaleString() : '-') },
            { header: 'Message', render: (e) => e.status?.message || '-' },
            {
              header: '',
              render: (e) => (
                <span className="rowactions">
                  <button disabled={e.status?.phase !== 'Ready'} onClick={() => download(e.metadata.name)}>
                    Download
                  </button>
                  <button className="danger" onClick={() => remove(e.metadata.name)}>
                    Delete
                  </button>
                </span>
              ),
            },
          ]}
        />
      </div>
    </div>
  );
}
