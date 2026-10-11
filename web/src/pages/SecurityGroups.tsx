// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api } from '../api';
import { NetworkSecurityGroup } from '../types';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

export default function SecurityGroups() {
  const [items, setItems] = useState<NetworkSecurityGroup[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<NetworkSecurityGroup[]>('/api/v1/security-groups').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">SECURITY GROUPS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(g) => g.metadata.name}
          emptyText={<EmptyHint what="security groups" command="kaironctl create securitygroup NAME …" />}
          columns={[
            { header: 'Name', render: (g) => g.metadata.name },
            { header: 'Group name', render: (g) => g.spec.groupName || g.metadata.name },
            { header: 'Priority', render: (g) => g.spec.priority ?? '-' },
            { header: 'Default allow', render: (g) => (g.spec.policy.defaultAllow ? 'yes' : 'no') },
            { header: 'Phase', render: (g) => g.status?.phase ?? '-' },
            { header: 'Applied on', render: (g) => g.status?.appliedOn ?? '-' },
          ]}
        />
      </div>
    </div>
  );
}
