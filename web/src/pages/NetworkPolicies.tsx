// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MachineNetworkPolicy } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

function formatSelector(selector?: Record<string, string>): string {
  const entries = Object.entries(selector || {});
  if (entries.length === 0) return '-';
  return entries.map(([k, v]) => `${k}=${v}`).join(', ');
}

function formatTarget(p: MachineNetworkPolicy): string {
  if (p.spec.machineName) return p.spec.machineName;
  return formatSelector(p.spec.selector);
}

export default function NetworkPolicies() {
  const [items, setItems] = useState<MachineNetworkPolicy[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<MachineNetworkPolicy[]>('/api/v1/network-policies').then(setItems).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">NETWORK POLICIES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(p) => p.metadata.name}
          emptyText={<EmptyHint what="network policies" hint="See docs/guides/network-policy.md." command="kaironctl create networkpolicy NAME …" />}
          columns={[
            { header: 'Name', render: (p) => p.metadata.name },
            { header: 'Target', render: (p) => formatTarget(p) },
            { header: 'Default allow', render: (p) => (p.spec.policy.defaultAllow ? 'yes' : 'no') },
            { header: 'Phase', render: (p) => p.status?.phase ?? '-' },
            { header: 'Synced', render: (p) => (p.status?.effectiveSynced ? 'yes' : 'no') },
          ]}
        />
      </div>
    </div>
  );
}
