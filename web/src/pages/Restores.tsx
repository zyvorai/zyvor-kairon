// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineSnapshotRestore } from '../types';
import ResourceTable from '../components/ResourceTable';
import { badgeClass } from '../lib/phase';
import { currentNamespace } from '../lib/namespace';
import EmptyHint from '../components/EmptyHint';

// Restores is the dashboard counterpart of Snapshots.tsx: create form plus
// a live table, following that page's shape (not ResourceTable's read-only
// five) since this one also creates. Unlike Snapshots, this page can also
// delete -- internal/kube.Client has always had DeleteMachineSnapshotRestore
// (kaironctl delete restore already used it), so there's no new mutation
// being introduced here, just the dashboard catching up. See
// internal/uiapi/restores.go for the backend side of both.
export default function Restores({ prefillSnapshot }: { prefillSnapshot: string }) {
  const [items, setItems] = useState<MachineSnapshotRestore[]>([]);
  const [snapshotName, setSnapshotName] = useState(prefillSnapshot);
  const [volumeName, setVolumeName] = useState('');
  const [targetClaimName, setTargetClaimName] = useState('');
  const [storageClassName, setStorageClassName] = useState('');
  const [storageSize, setStorageSize] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => setSnapshotName(prefillSnapshot), [prefillSnapshot]);

  const refresh = () =>
    api<MachineSnapshotRestore[]>('/api/v1/restores').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    if (!snapshotName || !targetClaimName) return;
    setBusy(true);
    setMsg('');
    try {
      await apiJSON('/api/v1/restores', 'POST', {
        snapshotName,
        volumeName,
        targetClaimName,
        storageClassName,
        storageSize,
      });
      setSnapshotName('');
      setVolumeName('');
      setTargetClaimName('');
      setStorageClassName('');
      setStorageSize('');
      await refresh();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  const remove = async (name: string) => {
    if (!confirm(`Delete MachineSnapshotRestore "${name}"? The PersistentVolumeClaim it already restored is left in place, not deleted.`)) return;
    try {
      await api(`/api/v1/restores/${currentNamespace()}/${encodeURIComponent(name)}`, { method: 'DELETE' });
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">CREATE RESTORE</span>
        <h3>Restore a snapshot into a new volume</h3>
        <form onSubmit={create}>
          <div className="formgrid">
            <label>
              Snapshot
              <input required value={snapshotName} onChange={(e) => setSnapshotName(e.target.value)} />
            </label>
            <label>
              Volume name
              <input value={volumeName} onChange={(e) => setVolumeName(e.target.value)} placeholder="required only if the snapshot covers more than one" />
            </label>
            <label>
              Target claim name
              <input required value={targetClaimName} onChange={(e) => setTargetClaimName(e.target.value)} />
            </label>
            <label>
              Storage class
              <input value={storageClassName} onChange={(e) => setStorageClassName(e.target.value)} placeholder="cluster default if empty" />
            </label>
            <label>
              Storage size
              <input value={storageSize} onChange={(e) => setStorageSize(e.target.value)} placeholder="snapshot's own restoreSize if empty" />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Creating...' : 'Create restore'}
            </button>
            {msg && <span className={msg.toLowerCase().includes('error') ? 'msg error' : 'msg'}>{msg}</span>}
          </div>
        </form>
      </div>

      <div className="card span4">
        <span className="eyebrow">RESTORES</span>
        <ResourceTable
          items={items}
          keyFn={(r) => r.metadata.name}
          emptyText={<EmptyHint what="restores" hint="Restore a snapshot from the Snapshots page, or: kaironctl restore SNAPSHOT --target-claim NAME." />}
          columns={[
            { header: 'Name', render: (r) => r.metadata.name },
            { header: 'Snapshot', render: (r) => r.spec.snapshotName },
            { header: 'Target claim', render: (r) => r.spec.targetClaimName },
            {
              header: 'Phase',
              render: (r) => <span className={badgeClass(r.status?.phase || '')}>{r.status?.phase || 'Pending'}</span>,
            },
            { header: 'Restored claim', render: (r) => r.status?.restoredClaimName || '-' },
            { header: 'Message', render: (r) => r.status?.message || '-' },
            {
              header: '',
              render: (r) => (
                <button className="danger" onClick={() => remove(r.metadata.name)}>
                  Delete
                </button>
              ),
            },
          ]}
        />
      </div>
    </div>
  );
}
