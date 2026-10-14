// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { badgeClass } from '../lib/phase';
import ResourceTable from '../components/ResourceTable';

interface Meta {
  name: string;
  namespace?: string;
}
interface Pool {
  metadata: Meta;
  spec: { replicas: number; warmPowerState?: string };
  status?: { replicas?: number; readyReplicas?: number; claimed?: number; message?: string };
}
interface Claim {
  metadata: Meta;
  spec: { poolName: string; reclaimPolicy?: string; ttlSeconds?: number };
  status?: {
    phase?: string;
    machineName?: string;
    bindMillis?: number;
    message?: string;
    usage?: { activeSeconds?: number; suspendedSeconds?: number; vcpuSeconds?: number };
  };
}

function seconds(s?: number): string {
  if (!s) return '-';
  if (s < 120) return `${s}s`;
  if (s < 7200) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

// Pools shows MachinePools (warm VM capacity) and the MachineClaims bound to
// them. Read-only: create/scale via kubectl or kaironctl.
export default function Pools() {
  const [pools, setPools] = useState<Pool[]>([]);
  const [claims, setClaims] = useState<Claim[]>([]);
  const [msg, setMsg] = useState('');

  useEffect(() => {
    const load = () => {
      api<Pool[]>('/api/v1/machinepools').then((r) => setPools(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
      api<Claim[]>('/api/v1/machineclaims').then((r) => setClaims(r ?? [])).catch((e) => setMsg(apiErrorText(e)));
    };
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE POOLS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={pools}
          keyFn={(p) => p.metadata.name}
          emptyText="No MachinePools yet."
          columns={[
            { header: 'Name', render: (p) => p.metadata.name },
            { header: 'Warm target', render: (p) => p.spec.replicas },
            { header: 'Warm state', render: (p) => p.spec.warmPowerState || 'Running' },
            { header: 'Ready', render: (p) => `${p.status?.readyReplicas ?? 0}/${p.status?.replicas ?? 0}` },
            { header: 'Claimed', render: (p) => p.status?.claimed ?? 0 },
            { header: 'Message', render: (p) => p.status?.message || '' },
          ]}
        />
      </div>
      <div className="card span4">
        <span className="eyebrow">MACHINE CLAIMS</span>
        <ResourceTable
          items={claims}
          keyFn={(c) => c.metadata.name}
          emptyText="No MachineClaims yet."
          columns={[
            { header: 'Name', render: (c) => c.metadata.name },
            { header: 'Pool', render: (c) => c.spec.poolName },
            { header: 'Phase', render: (c) => <span className={badgeClass(c.status?.phase || 'Pending')}>{c.status?.phase || 'Pending'}</span> },
            { header: 'Machine', render: (c) => c.status?.machineName || '-' },
            { header: 'Bind time', render: (c) => (c.status?.bindMillis ? `${c.status.bindMillis} ms` : '-') },
            { header: 'Active', render: (c) => seconds(c.status?.usage?.activeSeconds) },
            { header: 'Suspended', render: (c) => seconds(c.status?.usage?.suspendedSeconds) },
            { header: 'Reclaim', render: (c) => c.spec.reclaimPolicy || 'Delete' },
          ]}
        />
      </div>
    </div>
  );
}
