// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineSet } from '../types';
import ResourceTable from '../components/ResourceTable';
import { badgeClass } from '../lib/phase';
import { currentNamespace } from '../lib/namespace';
import EmptyHint from '../components/EmptyHint';

export default function MachineSets() {
  const [items, setItems] = useState<MachineSet[]>([]);
  const [msg, setMsg] = useState('');
  // Per-row draft replica count for the scale input, keyed by name -- kept
  // separate from `items` so typing a new value doesn't get clobbered by
  // the next 5s poll landing mid-edit, mirroring how Machines.tsx keeps
  // its own exec-form drafts out of the polled resource list.
  const [drafts, setDrafts] = useState<Record<string, string>>({});

  const refresh = () => api<MachineSet[]>('/api/v1/machinesets').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const remove = async (name: string) => {
    if (!confirm(`Delete MachineSet "${name}"? This also deletes every Machine it created.`)) return;
    try {
      await api(`/api/v1/machinesets/${currentNamespace()}/${encodeURIComponent(name)}`, { method: 'DELETE' });
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  const draftFor = (s: MachineSet) => drafts[s.metadata.name] ?? String(s.spec.replicas);

  const scale = async (s: MachineSet) => {
    const raw = draftFor(s);
    const replicas = Number(raw);
    if (!Number.isInteger(replicas) || replicas < 0) {
      setMsg(`Invalid replica count "${raw}": must be a non-negative integer.`);
      return;
    }
    try {
      await apiJSON(`/api/v1/machinesets/${currentNamespace()}/${encodeURIComponent(s.metadata.name)}/scale`, 'PATCH', { replicas });
      setDrafts((d) => {
        const next = { ...d };
        delete next[s.metadata.name];
        return next;
      });
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">MACHINE SETS</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(s) => s.metadata.name}
          emptyText={<EmptyHint what="MachineSets" command="kaironctl create machineset NAME --image PATH --replicas 3" />}
          columns={[
            { header: 'Name', render: (s) => s.metadata.name },
            { header: 'Replicas', render: (s) => s.spec.replicas },
            {
              header: 'Ready',
              render: (s) => {
                const ready = s.status?.readyReplicas ?? 0;
                const desired = s.status?.replicas ?? s.spec.replicas;
                return (
                  <span className={badgeClass(ready >= desired && desired > 0 ? 'Running' : 'Pending')}>
                    {ready}/{desired}
                  </span>
                );
              },
            },
            { header: 'Updated', render: (s) => s.status?.updatedReplicas ?? 0 },
            { header: 'Strategy', render: (s) => s.spec.strategy || 'RollingUpdate' },
            { header: 'Message', render: (s) => s.status?.message || '-' },
            {
              header: 'Scale',
              render: (s) => (
                <span style={{ display: 'inline-flex', gap: '0.4em', alignItems: 'center' }}>
                  <input
                    type="number"
                    min={0}
                    step={1}
                    value={draftFor(s)}
                    onChange={(e) =>
                      setDrafts((d) => ({ ...d, [s.metadata.name]: e.target.value }))
                    }
                    style={{ width: '4.5em' }}
                  />
                  <button onClick={() => scale(s)} disabled={Number(draftFor(s)) === s.spec.replicas}>
                    Scale
                  </button>
                </span>
              ),
            },
            {
              header: '',
              render: (s) => (
                <button className="danger" onClick={() => remove(s.metadata.name)}>
                  Delete
                </button>
              ),
            },
          ]}
        />
      </div>
    </div>
  );
}
