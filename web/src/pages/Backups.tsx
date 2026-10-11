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
  spec: { machineName: string; quiesce?: string; mode?: string; target?: { s3?: { bucket: string } } };
  status?: {
    phase?: string;
    message?: string;
    nodeName?: string;
    completionTime?: string;
    volumes?: unknown[];
    kind?: string;
    storedBytes?: number;
    changedBytes?: number;
    chain?: { id: string; sequence: number };
  };
}
interface ChainPoint {
  name: string;
  kind: string;
  phase?: string;
  sequence: number;
  depth: number;
  pointInTime?: string;
  storedBytes: number;
  changedBytes: number;
  retentionUntil?: string;
  deleting?: boolean;
}
interface Chain {
  id: string;
  storedBytes: number;
  points: ChainPoint[];
}
interface BackupRestore {
  metadata: Meta;
  spec: { backupName: string; machineName?: string };
  status?: { phase?: string; message?: string; machineName?: string; completionTime?: string };
}

const when = (t?: string) => (t ? new Date(t).toLocaleString() : '-');
const mib = (n?: number) => (n === undefined ? '-' : `${(n / 1048576).toFixed(1)} MiB`);
const kindOf = (b: Backup) => (b.spec.target?.s3 ? b.status?.kind || b.spec.mode || 'full' : 'local');
const MARK: Record<string, string> = { full: '●', incremental: '└', 'synthetic-full': '≡' };

// Backups lists MachineBackups and MachineBackupRestores (off-cluster backups
// through Atlas/S3) and restores or deletes a backup. A restore writes into
// the backed-up Machine, which must be halted; its disk half waits until it is.
export default function Backups() {
  const toast = useToast();
  const [backups, setBackups] = useState<Backup[]>([]);
  const [restores, setRestores] = useState<BackupRestore[]>([]);
  const [msg, setMsg] = useState('');
  const [armed, setArmed] = useState('');
  const [chainMachine, setChainMachine] = useState('');
  const [chains, setChains] = useState<Chain[]>([]);
  const load = () => {
    api<Backup[]>('/api/v1/machinebackups').then((r) => setBackups(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
    api<BackupRestore[]>('/api/v1/machinebackuprestores').then((r) => setRestores(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
  };

  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  useEffect(() => {
    if (!chainMachine) {
      setChains([]);
      return;
    }
    const get = () =>
      api<Chain[]>(`/api/v1/machines/${encodeURIComponent(currentNamespace())}/${encodeURIComponent(chainMachine)}/backup-chains`)
        .then((r) => setChains(r ?? []))
        .catch((e) => setMsg(apiErrorText(e)));
    get();
    const t = setInterval(get, 5000);
    return () => clearInterval(t);
  }, [chainMachine]);

  const nsOf = (b: Backup) => b.metadata.namespace || currentNamespace();
  const s3Machines = [...new Set(backups.filter((b) => b.spec.target?.s3).map((b) => b.spec.machineName))].sort();
  async function restorePoint(ns: string, name: string, machine: string) {
    setMsg('');
    try {
      await apiJSON(`/api/v1/machinebackups/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/restore`, 'POST', {});
      toast(`Restore of ${name} requested. Halt ${machine} so the disks can be written.`);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }
  const restore = (b: Backup) => restorePoint(nsOf(b), b.metadata.name, b.spec.machineName);
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
            { header: 'Kind', render: (b) => kindOf(b) },
            { header: 'Changed', render: (b) => (b.spec.target?.s3 && b.status?.phase === 'Succeeded' ? mib(b.status.changedBytes) : '-') },
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
        <span className="eyebrow">S3 BACKUP CHAINS</span>
        <p>
          <select value={chainMachine} onChange={(e) => setChainMachine(e.target.value)} aria-label="Machine">
            <option value="">Choose a machine</option>
            {s3Machines.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </p>
        {chains.map((c) => (
          <div key={c.id}>
            <h4>
              Chain {c.id} · {c.points.length} backups · {mib(c.storedBytes)} stored
            </h4>
            <ResourceTable
              items={c.points}
              keyFn={(p) => p.name}
              emptyText=""
              columns={[
                { header: 'Point', render: (p) => <span style={{ paddingLeft: `${p.depth}em` }}>{`${MARK[p.kind] ?? '·'} ${p.name}`}</span> },
                { header: '#', render: (p) => p.sequence },
                { header: 'Kind', render: (p) => p.kind },
                { header: 'Phase', render: (p) => <span className={badgeClass(p.phase || 'Pending')}>{p.phase || 'Pending'}</span> },
                { header: 'Point in time', render: (p) => when(p.pointInTime) },
                { header: 'Stored', render: (p) => mib(p.storedBytes) },
                { header: 'Changed', render: (p) => mib(p.changedBytes) },
                { header: 'Locked until', render: (p) => (p.retentionUntil ? when(p.retentionUntil) : '-') },
                {
                  header: '',
                  render: (p) => (
                    <button disabled={p.phase !== 'Succeeded' || p.deleting} title="Restore this point (the Machine must be halted)" onClick={() => restorePoint(currentNamespace(), p.name, chainMachine)}>
                      Restore
                    </button>
                  ),
                },
              ]}
            />
          </div>
        ))}
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
