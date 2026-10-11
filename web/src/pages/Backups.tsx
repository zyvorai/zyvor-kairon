// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
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
// through Atlas/S3). Read-only; create with kaironctl backup / kubectl.
export default function Backups() {
  const [backups, setBackups] = useState<Backup[]>([]);
  const [restores, setRestores] = useState<BackupRestore[]>([]);
  const [msg, setMsg] = useState('');

  useEffect(() => {
    const load = () => {
      api<Backup[]>('/api/v1/machinebackups').then(setBackups).catch((e) => setMsg(apiErrorText(e)));
      api<BackupRestore[]>('/api/v1/machinebackuprestores').then(setRestores).catch((e) => setMsg(apiErrorText(e)));
    };
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

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
