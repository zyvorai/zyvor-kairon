// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { MachineInstanceType } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

export default function InstanceTypes() {
  const [items, setItems] = useState<MachineInstanceType[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<MachineInstanceType[]>('/api/v1/instancetypes').then(setItems).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE INSTANCE TYPES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(t) => t.metadata.name}
          emptyText={<EmptyHint what="instance types" command="kaironctl create instancetype NAME --cpu 2 --memory 4Gi" />}
          columns={[
            { header: 'Name', render: (t) => t.metadata.name },
            { header: 'CPU', render: (t) => t.spec.resources.cpu },
            { header: 'Memory', render: (t) => t.spec.resources.memory },
            { header: 'Max CPU', render: (t) => t.spec.resources.maxCpu || '-' },
            { header: 'Max memory', render: (t) => t.spec.resources.maxMemory || '-' },
            { header: 'Hugepages', render: (t) => (t.spec.resources.hugepages ? 'true' : 'false') },
            { header: 'CPU pinning', render: (t) => (t.spec.resources.cpuPinning ? 'true' : 'false') },
          ]}
        />
      </div>
    </div>
  );
}
