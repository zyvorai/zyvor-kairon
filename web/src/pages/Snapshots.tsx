// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineSnapshot } from '../types';
import { badgeClass } from '../lib/phase';
import { allNamespaces, currentNamespace } from '../lib/namespace';

export default function Snapshots({ prefillMachine, onRestore }: { prefillMachine: string; onRestore: (snapshot: string) => void }) {
  const [items, setItems] = useState<MachineSnapshot[]>([]);
  const [machine, setMachine] = useState(prefillMachine);
  const [volumeClass, setVolumeClass] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const showNs = allNamespaces();
  const here = currentNamespace();

  useEffect(() => setMachine(prefillMachine), [prefillMachine]);

  const refresh = () =>
    api<MachineSnapshot[]>('/api/v1/snapshots').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    if (!machine) return;
    setBusy(true);
    setMsg('');
    try {
      await apiJSON('/api/v1/snapshots', 'POST', { machine, class: volumeClass });
      setMachine('');
      setVolumeClass('');
      await refresh();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">CREATE SNAPSHOT</span>
        <h3>Snapshot a machine</h3>
        <form onSubmit={create}>
          <div className="formgrid">
            <label>
              Machine
              <input required value={machine} onChange={(e) => setMachine(e.target.value)} />
            </label>
            <label>
              VolumeSnapshotClass
              <input value={volumeClass} onChange={(e) => setVolumeClass(e.target.value)} placeholder="cluster default if empty" />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Creating...' : 'Create snapshot'}
            </button>
            {msg && <span className={msg.toLowerCase().includes('error') ? 'msg error' : 'msg'}>{msg}</span>}
          </div>
        </form>
      </div>

      <div className="card span4">
        <span className="eyebrow">SNAPSHOTS</span>
        <table className="datatable">
          <thead>
            <tr>
              {showNs && <th>Namespace</th>}
              <th>Name</th>
              <th>Machine</th>
              <th>Phase</th>
              <th>Ready</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {items.map((s) => (
              <tr key={`${s.metadata.namespace || ''}/${s.metadata.name}`}>
                {showNs && <td>{s.metadata.namespace}</td>}
                <td>{s.metadata.name}</td>
                <td>{s.spec.machineName}</td>
                <td>
                  <span className={badgeClass(s.status?.phase || '')}>{s.status?.phase || 'Pending'}</span>
                </td>
                <td>{s.status?.readyToUse ? 'true' : 'false'}</td>
                <td>
                  {s.status?.readyToUse && (!s.metadata.namespace || s.metadata.namespace === here) && <button onClick={() => onRestore(s.metadata.name)}>Restore</button>}
                </td>
              </tr>
            ))}
            {items.length === 0 && (
              <tr>
                <td colSpan={showNs ? 6 : 5} className="msg">
                  No snapshots yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
