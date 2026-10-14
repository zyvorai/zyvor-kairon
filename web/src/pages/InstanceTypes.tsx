// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MachineInstanceType, MachinePreference } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

type Scoped<T> = T & { scope: 'Namespace' | 'Cluster' };

export default function InstanceTypes() {
  const [items, setItems] = useState<Scoped<MachineInstanceType>[]>([]);
  const [prefs, setPrefs] = useState<Scoped<MachinePreference>[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    Promise.all([
      api<MachineInstanceType[]>('/api/v1/instancetypes'),
      api<MachineInstanceType[]>('/api/v1/cluster-instancetypes').catch(() => []),
      api<MachinePreference[]>('/api/v1/preferences'),
      api<MachinePreference[]>('/api/v1/cluster-preferences').catch(() => []),
    ])
      .then(([ns, cluster, nsPrefs, clusterPrefs]) => {
        setItems([
          ...(ns ?? []).map((t) => ({ ...t, scope: 'Namespace' as const })),
          ...(cluster ?? []).map((t) => ({ ...t, scope: 'Cluster' as const })),
        ]);
        setPrefs([
          ...(nsPrefs ?? []).map((p) => ({ ...p, scope: 'Namespace' as const })),
          ...(clusterPrefs ?? []).map((p) => ({ ...p, scope: 'Cluster' as const })),
        ]);
        setMsg('');
      })
      .catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const yes = (b?: boolean) => (b ? 'true' : 'false');

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE INSTANCE TYPES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(t) => t.scope + '/' + t.metadata.name}
          emptyText={<EmptyHint what="instance types" command="kaironctl create instancetype NAME --cpu 2 --memory 4Gi" />}
          columns={[
            { header: 'Name', render: (t) => t.metadata.name },
            { header: 'Scope', render: (t) => t.scope },
            { header: 'CPU', render: (t) => t.spec.resources.cpu },
            { header: 'Memory', render: (t) => t.spec.resources.memory },
            { header: 'Max CPU', render: (t) => t.spec.resources.maxCpu || '-' },
            { header: 'Max memory', render: (t) => t.spec.resources.maxMemory || '-' },
            { header: 'Hugepages', render: (t) => yes(t.spec.resources.hugepages) },
            { header: 'CPU pinning', render: (t) => yes(t.spec.resources.cpuPinning) },
          ]}
        />
      </div>
      <div className="card span4">
        <span className="eyebrow">MACHINE PREFERENCES</span>
        <ResourceTable
          items={prefs}
          keyFn={(p) => p.scope + '/' + p.metadata.name}
          emptyText={<EmptyHint what="preferences" command="kubectl apply -f preference.yaml" />}
          columns={[
            { header: 'Name', render: (p) => p.metadata.name },
            { header: 'Scope', render: (p) => p.scope },
            { header: 'Guest OS', render: (p) => p.spec.guestOS || '-' },
            { header: 'Backend', render: (p) => p.spec.backend || '-' },
            { header: 'Arch', render: (p) => p.spec.architecture || '-' },
            { header: 'Secure boot', render: (p) => yes(p.spec.secureBoot) },
            { header: 'TPM', render: (p) => yes(p.spec.tpm) },
            { header: 'Guest agent', render: (p) => yes(p.spec.guestAgent) },
          ]}
        />
      </div>
    </div>
  );
}
