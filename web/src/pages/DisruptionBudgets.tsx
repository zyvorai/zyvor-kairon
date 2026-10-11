// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MachineDisruptionBudget } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

function formatSelector(selector: Record<string, string>): string {
  const entries = Object.entries(selector || {});
  if (entries.length === 0) return '-';
  return entries.map(([k, v]) => `${k}=${v}`).join(', ');
}

export default function DisruptionBudgets() {
  const [items, setItems] = useState<MachineDisruptionBudget[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<MachineDisruptionBudget[]>('/api/v1/disruption-budgets').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE DISRUPTION BUDGETS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(b) => b.metadata.name}
          emptyText={<EmptyHint what="MachineDisruptionBudgets" hint="Budgets limit concurrent voluntary disruptions; see docs/guides/machine-disruption-budgets.md." />}
          columns={[
            { header: 'Name', render: (b) => b.metadata.name },
            { header: 'Selector', render: (b) => formatSelector(b.spec.selector) },
            { header: 'Min available / max unavailable', render: (b) => b.spec.minAvailable || b.spec.maxUnavailable || '-' },
            { header: 'Expected', render: (b) => b.status?.expectedMachines ?? 0 },
            { header: 'Healthy', render: (b) => b.status?.currentHealthy ?? 0 },
            { header: 'Desired healthy', render: (b) => b.status?.desiredHealthy ?? 0 },
            { header: 'Disruptions allowed', render: (b) => b.status?.disruptionsAllowed ?? 0 },
          ]}
        />
      </div>
    </div>
  );
}
