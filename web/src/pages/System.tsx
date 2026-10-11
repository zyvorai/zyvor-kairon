// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { badgeClass } from '../lib/phase';
import { relativeTime } from '../lib/timeline';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

export interface PodRow {
  namespace: string;
  name: string;
  phase: string;
  node?: string;
  ip?: string;
  ready: boolean;
  restarts: number;
  startTime?: string;
  component?: string;
}

interface ComponentHealth {
  component: string;
  pods: number;
  ready: number;
  restarts: number;
  healthy: boolean;
}

export function podStatus(p: PodRow): string {
  if (p.phase !== 'Running') return p.phase || 'Unknown';
  return p.ready ? 'Running' : 'NotReady';
}

// System is Kairon's own control-plane and node-agent pods, with an option
// to widen to every pod the caller may see: the dashboard form of
// `kaironctl doctor`'s workload checks.
export default function System() {
  const [health, setHealth] = useState<ComponentHealth[]>([]);
  const [pods, setPods] = useState<PodRow[]>([]);
  const [all, setAll] = useState(false);
  const [msg, setMsg] = useState('');

  useEffect(() => {
    let live = true;
    const load = () => {
      api<ComponentHealth[]>('/api/v1/health').then((r) => live && setHealth(r ?? [])).catch((e) => live && setMsg(apiErrorText(e)));
      api<PodRow[]>(`/api/v1/pods${all ? '?scope=all' : ''}`)
        .then((r) => live && (setPods(r ?? []), setMsg('')))
        .catch((e) => live && setMsg(apiErrorText(e)));
    };
    load();
    const t = setInterval(load, 10000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [all]);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">COMPONENTS</span>
        {health.length === 0 && !msg && <EmptyHint what="Kairon components" namespaced={false} hint="No pods labelled app.kubernetes.io/name=kairon-* were found." />}
        <div className="healthgrid">
          {health.map((h) => (
            <div key={h.component} className={'healthcard ' + (h.healthy ? 'ok' : 'bad')}>
              <strong>{h.component}</strong>
              <span className="healthcount">{h.ready}/{h.pods} ready</span>
              {h.restarts > 0 && <span className="usageHint">{h.restarts} restarts</span>}
            </div>
          ))}
        </div>
      </div>
      <div className="card span4">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
          <span className="eyebrow">PODS</span>
          <label className="usageHint">
            <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} /> Show all pods
          </label>
        </div>
        {msg && <p className="msg error">{msg}</p>}
        <ResourceTable
          items={pods}
          keyFn={(p) => `${p.namespace}/${p.name}`}
          emptyText={<EmptyHint what="pods" namespaced={false} hint="Pod listing needs the kairon-ui ClusterRole's pods get/list rule." />}
          columns={[
            { header: 'Name', render: (p) => p.name },
            { header: 'Namespace', render: (p) => p.namespace },
            { header: 'Component', render: (p) => p.component ?? '-' },
            { header: 'Status', render: (p) => <span className={badgeClass(podStatus(p))}>{podStatus(p)}</span> },
            { header: 'Restarts', render: (p) => p.restarts },
            { header: 'Node', render: (p) => p.node ?? '-' },
            { header: 'IP', render: (p) => p.ip ?? '-' },
            { header: 'Age', render: (p) => (p.startTime ? relativeTime(p.startTime).replace(' ago', '') : '-') },
          ]}
        />
      </div>
    </div>
  );
}
