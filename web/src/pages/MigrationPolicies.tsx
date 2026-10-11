// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MigrationPolicy } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

function formatSelector(selector: Record<string, string>): string {
  const entries = Object.entries(selector || {});
  if (entries.length === 0) return '-';
  return entries.map(([k, v]) => `${k}=${v}`).join(', ');
}

export default function MigrationPolicies() {
  const [items, setItems] = useState<MigrationPolicy[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<MigrationPolicy[]>('/api/v1/migration-policies').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MIGRATION POLICIES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(p) => p.metadata.name}
          emptyText={<EmptyHint what="migration policies" hint="See docs/guides/migration-policies.md." command="kaironctl create migrationpolicy NAME …" />}
          columns={[
            { header: 'Name', render: (p) => p.metadata.name },
            { header: 'Selector', render: (p) => formatSelector(p.spec.selector) },
            { header: 'Bandwidth (Mbps)', render: (p) => p.spec.bandwidthMbps ?? '-' },
            { header: 'Max concurrent', render: (p) => (p.spec.maxConcurrent ? p.spec.maxConcurrent : 'unlimited') },
            { header: 'Active migrations', render: (p) => p.status?.activeMigrations ?? 0 },
          ]}
        />
      </div>
    </div>
  );
}
