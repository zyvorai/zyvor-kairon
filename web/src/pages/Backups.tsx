// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText, apiJSON } from '../api';
import { currentNamespace } from '../lib/namespace';
import { useToast } from '../components/Toast';
import { badgeClass } from '../lib/phase';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

interface Meta {
  name: string;
  namespace?: string;
  creationTimestamp?: string;
}
interface Backup {
  metadata: Meta;
  spec: { machineName: string; quiesce?: string };
  status?: { phase?: string; message?: string; nodeName?: string; completionTime?: string; volumes?: unknown[] };
}
interface BackupRestore {
  metadata: Meta;
  spec: { backupName: string; machineName?: string };
  status?: { phase?: string; message?: string; machineName?: string; completionTime?: string };
}

const when = (t?: string) => (t ? new Date(t).toLocaleString() : '-');

// Backups lists MachineBackups and MachineBackupRestores (off-cluster backups
// through Atlas/S3) and restores or deletes a backup. A restore writes into
// the backed-up Machine, which must be halted; its disk half waits until it is.
export default function Backups() {
  const toast = useToast();
  const [backups, setBackups] = useState<Backup[]>([]);
  const [restores, setRestores] = useState<BackupRestore[]>([]);
  const [msg, setMsg] = useState('');
  const [armed, setArmed] = useState('');
  const load = () => {
    api<Backup[]>('/api/v1/machinebackups').then((r) => setBackups(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
    api<BackupRestore[]>('/api/v1/machinebackuprestores').then((r) => setRestores(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
  };

  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  const nsOf = (b: Backup) => b.metadata.namespace || currentNamespace();
  async function restore(b: Backup) {
    setMsg('');
    try {
      await apiJSON(`/api/v1/machinebackups/${encodeURIComponent(nsOf(b))}/${encodeURIComponent(b.metadata.name)}/restore`, 'POST', {});
      toast(`Restore of ${b.metadata.name} requested. Halt ${b.spec.machineName} so the disks can be written.`);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }
  async function remove(b: Backup) {
    if (armed !== b.metadata.name) {
      setArmed(b.metadata.name);
      return;
    }
    setArmed('');
    setMsg('');
    try {
      await api(`/api/v1/machinebackups/${encodeURIComponent(nsOf(b))}/${encodeURIComponent(b.metadata.name)}`, { method: 'DELETE' });
      toast(`Backup ${b.metadata.name} deleted`);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE BACKUPS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={backups}
          keyFn={(b) => b.metadata.name}
          emptyText={<EmptyHint what="MachineBackups" hint="Off-cluster backups; see docs/guides/machine-backup.md." />}
          columns={[
            { header: 'Name', render: (b) => b.metadata.name },
            { header: 'Machine', render: (b) => b.spec.machineName },
            { header: 'Phase', render: (b) => <span className={badgeClass(b.status?.phase || 'Pending')}>{b.status?.phase || 'Pending'}</span> },
            { header: 'Quiesce', render: (b) => b.spec.quiesce || '-' },
            { header: 'Node', render: (b) => b.status?.nodeName || '-' },
            { header: 'Completed', render: (b) => when(b.status?.completionTime) },
            { header: 'Message', render: (b) => b.status?.message || '' },
            {
              header: '',
              render: (b) => (
                <span className="rowactions">
                  <button disabled={b.status?.phase !== 'Succeeded'} title="Restore into the backed-up Machine (it must be halted)" onClick={() => restore(b)}>Restore</button>
                  <button className="danger" onClick={() => remove(b)}>{armed === b.metadata.name ? 'Confirm delete' : 'Delete'}</button>
                </span>
              ),
            },
          ]}
        />
      </div>
      <div className="card span4">
        <span className="eyebrow">BACKUP RESTORES</span>
        <ResourceTable
          items={restores}
          keyFn={(r) => r.metadata.name}
          emptyText="No MachineBackupRestores yet."
          columns={[
            { header: 'Name', render: (r) => r.metadata.name },
            { header: 'Backup', render: (r) => r.spec.backupName },
            { header: 'Machine', render: (r) => r.status?.machineName || r.spec.machineName || '-' },
            { header: 'Phase', render: (r) => <span className={badgeClass(r.status?.phase || 'Pending')}>{r.status?.phase || 'Pending'}</span> },
            { header: 'Completed', render: (r) => when(r.status?.completionTime) },
            { header: 'Message', render: (r) => r.status?.message || '' },
          ]}
        />
      </div>
    </div>
  );
}
