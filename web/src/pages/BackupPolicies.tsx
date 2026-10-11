// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText, apiJSON } from '../api';
import { currentNamespace } from '../lib/namespace';
import { useToast } from '../components/Toast';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

interface PolicyMachine {
  machineName: string;
  chainID?: string;
  lastFull?: string;
  lastIncremental?: string;
  nextRun?: string;
  skipped?: string;
  lastError?: string;
  retentionHeld?: string[];
}
interface BackupPolicy {
  metadata: { name: string; namespace?: string };
  spec: {
    selector: Record<string, string>;
    mode?: string;
    fullEvery?: string;
    incrementalEvery: string;
    target: { s3?: { endpoint: string; bucket: string; prefix?: string } };
    immutableFor?: string;
    encryption?: { keySecretRef: string };
    retention?: { hourly?: number; daily?: number; monthly?: number };
    suspend?: boolean;
  };
  status?: { machines?: PolicyMachine[] };
}

const when = (t?: string) => (t ? new Date(t).toLocaleString() : '-');

const note = (m: PolicyMachine) =>
  [m.lastError && `error: ${m.lastError}`, m.skipped, m.retentionHeld?.length ? `Object Lock holds ${m.retentionHeld.join(', ')}` : '']
    .filter(Boolean)
    .join('; ');

// BackupPolicies lists MachineBackupPolicies (scheduled incremental S3
// backups with GFS retention) and each selected Machine's chain state.
// Policies are created with kaironctl backuppolicy create or YAML.
export default function BackupPolicies() {
  const toast = useToast();
  const [policies, setPolicies] = useState<BackupPolicy[]>([]);
  const [msg, setMsg] = useState('');
  const [armed, setArmed] = useState('');
  const load = () => {
    api<BackupPolicy[]>('/api/v1/machinebackuppolicies').then((r) => setPolicies(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
  };

  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  const path = (p: BackupPolicy) => `/api/v1/machinebackuppolicies/${encodeURIComponent(p.metadata.namespace || currentNamespace())}/${encodeURIComponent(p.metadata.name)}`;
  async function suspend(p: BackupPolicy, on: boolean) {
    setMsg('');
    try {
      await apiJSON(`${path(p)}/suspend`, 'POST', { suspend: on });
      toast(`${p.metadata.name} ${on ? 'suspended' : 'resumed'}`);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }
  async function remove(p: BackupPolicy) {
    if (armed !== p.metadata.name) {
      setArmed(p.metadata.name);
      return;
    }
    setArmed('');
    try {
      await api(path(p), { method: 'DELETE' });
      toast(`Policy ${p.metadata.name} deleted; its backups stay`);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  }

  const rows = policies.flatMap((p) => (p.status?.machines ?? []).map((m) => ({ policy: p.metadata.name, ...m })));
  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">BACKUP POLICIES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={policies}
          keyFn={(p) => p.metadata.name}
          emptyText={<EmptyHint what="MachineBackupPolicies" hint="Incremental S3 backups; see docs/guides/machine-backup-incremental.md." />}
          columns={[
            { header: 'Name', render: (p) => p.metadata.name },
            { header: 'Selector', render: (p) => Object.entries(p.spec.selector).map(([k, v]) => `${k}=${v}`).join(', ') },
            { header: 'Mode', render: (p) => p.spec.mode || 'incremental' },
            { header: 'Every', render: (p) => `${p.spec.incrementalEvery}${p.spec.fullEvery ? ` (full ${p.spec.fullEvery})` : ''}` },
            { header: 'Target', render: (p) => (p.spec.target.s3 ? `s3://${p.spec.target.s3.bucket}/${p.spec.target.s3.prefix ?? ''}` : '-') },
            {
              header: 'Retention',
              render: (p) => `${p.spec.retention?.hourly ?? 0}h ${p.spec.retention?.daily ?? 0}d ${p.spec.retention?.monthly ?? 0}m${p.spec.immutableFor ? `, locked ${p.spec.immutableFor}` : ''}`,
            },
            { header: 'Encrypted', render: (p) => (p.spec.encryption ? 'yes' : 'no') },
            { header: 'Suspended', render: (p) => (p.spec.suspend ? 'yes' : 'no') },
            {
              header: '',
              render: (p) => (
                <span className="rowactions">
                  <button onClick={() => suspend(p, !p.spec.suspend)}>{p.spec.suspend ? 'Resume' : 'Suspend'}</button>
                  <button className="danger" onClick={() => remove(p)}>{armed === p.metadata.name ? 'Confirm delete' : 'Delete'}</button>
                </span>
              ),
            },
          ]}
        />
      </div>
      <div className="card span4">
        <span className="eyebrow">MACHINES</span>
        <ResourceTable
          items={rows}
          keyFn={(r) => `${r.policy}/${r.machineName}`}
          emptyText="No Machines selected yet."
          columns={[
            { header: 'Policy', render: (r) => r.policy },
            { header: 'Machine', render: (r) => r.machineName },
            { header: 'Chain', render: (r) => r.chainID || '-' },
            { header: 'Last full', render: (r) => when(r.lastFull) },
            { header: 'Last incremental', render: (r) => when(r.lastIncremental) },
            { header: 'Next run', render: (r) => when(r.nextRun) },
            { header: 'Note', render: (r) => note(r) },
          ]}
        />
      </div>
    </div>
  );
}
