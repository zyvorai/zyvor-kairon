// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { Machine } from '../types';
import { netHealth, summarizeNetwork } from '../lib/network';
import { useLiveChanges } from '../lib/live';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

const LABEL = { ok: 'Synced', warn: 'Needs attention', idle: 'No dataplane' } as const;
const BADGE = { ok: 'badge badge-ok', warn: 'badge badge-warn', idle: 'badge badge-idle' } as const;

// A fleet view of the VM-edge dataplane, built from the status
// kairon-node already projects onto every Machine (dataplane attach,
// policy sync, identity, anti-spoof, conntrack restore). Per-VM flows and
// drop reasons stay in the Machine inspector's Network panel.
export default function NetworkOverview() {
  const [items, setItems] = useState<Machine[]>([]);
  const [msg, setMsg] = useState('');
  const load = () =>
    api<Machine[]>('/api/v1/machines').then((r) => (setItems(r ?? []), setMsg(''))).catch((e) => setMsg(apiErrorText(e)));

  const live = useLiveChanges((c) => c.kind === 'Machine' && void load());
  useEffect(() => {
    load();
    const t = setInterval(load, live ? 30000 : 5000);
    return () => clearInterval(t);
  }, [live]);

  const s = summarizeNetwork(items);
  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">DATAPLANE</span>
        <div className="healthgrid">
          <div className="healthcard"><strong>{s.attached}/{s.total}</strong><span className="healthcount">dataplane attached</span></div>
          <div className="healthcard"><strong>{s.policySynced}</strong><span className="healthcount">policy synced</span></div>
          <div className="healthcard"><strong>{s.antiSpoof}</strong><span className="healthcount">anti-spoof on</span></div>
          <div className={'healthcard ' + (s.needsAttention ? 'bad' : 'ok')}><strong>{s.needsAttention}</strong><span className="healthcount">need attention</span></div>
        </div>
        {s.byPolicy.length > 0 && (
          <p className="usageHint">Policies: {s.byPolicy.map((p) => `${p.policy} ×${p.count}`).join(' · ')}</p>
        )}
      </div>
      <div className="card span4">
        <span className="eyebrow">MACHINES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(m) => `${m.metadata.namespace ?? ''}/${m.metadata.name}`}
          emptyText={<EmptyHint what="machines" namespaced />}
          columns={[
            { header: 'Name', render: (m) => m.metadata.name },
            { header: 'Dataplane', render: (m) => <span className={BADGE[netHealth(m)]}>{LABEL[netHealth(m)]}</span> },
            { header: 'Policy', render: (m) => m.status?.network?.edge?.policyName ?? '-' },
            { header: 'Identity', render: (m) => m.status?.network?.edge?.identity ?? m.status?.network?.dataplane?.identity ?? '-' },
            { header: 'Guest IP', render: (m) => (m.status?.network?.guestIPs ?? (m.status?.guestIP ? [m.status.guestIP] : [])).join(', ') || '-' },
            { header: 'IP source', render: (m) => m.status?.network?.edge?.guestIPSource ?? '-' },
            { header: 'Anti-spoof', render: (m) => (m.status?.network?.edge?.antiSpoof ? 'on' : '-') },
            { header: 'Conntrack restored', render: (m) => m.status?.network?.edge?.conntrackRestored ?? '-' },
            { header: 'Last blackhole', render: (m) => (m.status?.network?.edge?.blackholeWindowMs ? `${m.status.network.edge.blackholeWindowMs} ms` : '-') },
          ]}
        />
      </div>
    </div>
  );
}
