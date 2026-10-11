// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MachineQuota } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

export default function Quotas() {
  const [items, setItems] = useState<MachineQuota[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () => api<MachineQuota[]>('/api/v1/quotas').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE QUOTAS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(q) => q.metadata.name}
          emptyText={<EmptyHint what="MachineQuotas" hint="Quotas cap CPU, memory and Machine count per namespace; see docs/guides/machine-quotas.md." command="kaironctl create quota NAME …" />}
          columns={[
            { header: 'Name', render: (q) => q.metadata.name },
            { header: 'Max machines', render: (q) => q.spec.maxMachines ?? '-' },
            { header: 'Max total CPU', render: (q) => q.spec.maxTotalCpu || '-' },
            { header: 'Max total memory', render: (q) => q.spec.maxTotalMemory || '-' },
            { header: 'Used machines', render: (q) => q.status?.usedMachines ?? 0 },
            { header: 'Used CPU cores', render: (q) => q.status?.usedTotalCpuCores ?? 0 },
            { header: 'Used memory (MiB)', render: (q) => q.status?.usedTotalMemoryMiB ?? 0 },
          ]}
        />
      </div>
    </div>
  );
}
