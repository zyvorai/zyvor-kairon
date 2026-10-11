// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineSnapshotSchedule } from '../types';
import ResourceTable from '../components/ResourceTable';
import { badgeClass } from '../lib/phase';
import { currentNamespace } from '../lib/namespace';
import EmptyHint from '../components/EmptyHint';

function formatSelector(selector: Record<string, string>): string {
  const entries = Object.entries(selector || {});
  if (entries.length === 0) return '-';
  return entries.map(([k, v]) => `${k}=${v}`).join(', ');
}

// formatNextRun mirrors kaironctl's own formatNextRun (internal/kaironctl/kaironctl.go):
// status.nextRunTime is only ever updated when a schedule actually fires, so
// a schedule suspended sometime after its last run would otherwise show an
// already-passed timestamp as if a run were still pending. Checking the
// live spec.suspend flag here at render time means that can't happen.
function formatNextRun(s: MachineSnapshotSchedule): string {
  if (s.spec.suspend) return 'suspended';
  if (!s.status?.nextRunTime) return 'pending';
  return new Date(s.status.nextRunTime).toLocaleString();
}

export default function SnapshotSchedules() {
  const [items, setItems] = useState<MachineSnapshotSchedule[]>([]);
  const [msg, setMsg] = useState('');

  const refresh = () =>
    api<MachineSnapshotSchedule[]>('/api/v1/snapshot-schedules').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  const toggleSuspend = async (s: MachineSnapshotSchedule) => {
    try {
      await apiJSON(`/api/v1/snapshot-schedules/${currentNamespace()}/${encodeURIComponent(s.metadata.name)}/suspend`, 'PATCH', {
        suspend: !s.spec.suspend,
      });
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  const runNow = async (s: MachineSnapshotSchedule) => {
    try {
      await apiJSON(`/api/v1/snapshot-schedules/${currentNamespace()}/${encodeURIComponent(s.metadata.name)}/trigger`, 'POST', {});
      setMsg('');
      refresh();
    } catch (e) {
      setMsg(String(e));
    }
  };

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">SNAPSHOT SCHEDULES</span>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={items}
          keyFn={(s) => s.metadata.name}
          emptyText={<EmptyHint what="snapshot schedules" command="kaironctl create snapshotschedule NAME --selector k=v --daily-at 02:30" />}
          columns={[
            { header: 'Name', render: (s) => s.metadata.name },
            { header: 'Selector', render: (s) => formatSelector(s.spec.selector) },
            { header: 'Interval', render: (s) => `${s.spec.intervalSeconds}s` },
            { header: 'Keep last', render: (s) => (s.spec.keepLast ? s.spec.keepLast : 'unbounded') },
            {
              header: 'Status',
              render: (s) => (
                <span className={badgeClass(s.spec.suspend ? 'Paused' : 'Running')}>
                  {s.spec.suspend ? 'Suspended' : 'Active'}
                </span>
              ),
            },
            { header: 'Last run', render: (s) => (s.status?.lastRunTime ? new Date(s.status.lastRunTime).toLocaleString() : 'never') },
            { header: 'Last count', render: (s) => s.status?.lastRunSnapshotCount ?? 0 },
            { header: 'Next run', render: (s) => formatNextRun(s) },
            { header: 'Last error', render: (s) => s.status?.lastRunError || '-' },
            {
              header: '',
              render: (s) => (
                <span className="rowactions">
                  <button onClick={() => runNow(s)} title="Snapshot every matching Machine now, regardless of suspend or interval">Run now</button>
                  <button onClick={() => toggleSuspend(s)}>{s.spec.suspend ? 'Resume' : 'Suspend'}</button>
                </span>
              ),
            },
          ]}
        />
      </div>
    </div>
  );
}
