// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react';
import { api, apiErrorText, apiJSON } from '../api';
import { KaironNode, Machine } from '../types';
import { useLiveChanges } from '../lib/live';
import { currentNamespace } from '../lib/namespace';
import { formatHash } from '../lib/route';
import { buildLanes, canMigrateTo, Lane, TopoPod, UNSCHEDULED } from '../lib/topology';
import { useToast } from '../components/Toast';
import EmptyHint from '../components/EmptyHint';

const TONE: Record<string, string> = {
  Running: 'ok', Succeeded: 'ok',
  Failed: 'bad', Error: 'bad',
  Paused: 'warn', Pending: 'warn', Halted: 'warn', Unknown: 'warn',
};

// Nodes as lanes, VMs as dots, pods as squares. A Running VM can be dragged
// onto another Ready node; that stages a migration the operator confirms,
// it never moves anything on drop alone.
export default function Topology() {
  const toast = useToast();
  const [machines, setMachines] = useState<Machine[]>([]);
  const [nodes, setNodes] = useState<KaironNode[]>([]);
  const [pods, setPods] = useState<TopoPod[]>([]);
  const [msg, setMsg] = useState('');
  const [drag, setDrag] = useState<Machine | null>(null);
  const [over, setOver] = useState('');
  const [pending, setPending] = useState<{ machine: Machine; node: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const liveTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const load = () => {
    api<Machine[]>('/api/v1/machines').then((r) => (setMachines(r ?? []), setMsg(''))).catch((e) => setMsg(apiErrorText(e)));
    api<KaironNode[]>('/api/v1/nodes').then((r) => setNodes(r ?? [])).catch(() => setNodes([]));
    api<TopoPod[]>('/api/v1/pods?scope=all').then((r) => setPods(r ?? [])).catch(() => setPods([]));
  };
  const live = useLiveChanges(() => {
    clearTimeout(liveTimer.current);
    liveTimer.current = setTimeout(load, 300);
  });
  useEffect(() => () => clearTimeout(liveTimer.current), []);
  useEffect(() => {
    load();
    const t = setInterval(load, live ? 30000 : 6000);
    return () => clearInterval(t);
  }, [live]);

  const lanes = buildLanes(machines, nodes, pods);
  const ns = currentNamespace();
  const movable = (m: Machine) => (m.metadata.namespace ?? ns) === ns && m.status?.phase === 'Running';

  async function confirmMove() {
    if (!pending) return;
    setBusy(true);
    try {
      await apiJSON('/api/v1/migrations', 'POST', { machine: pending.machine.metadata.name, targetNode: pending.node, strategy: 'auto' });
      toast(`Migrating ${pending.machine.metadata.name} to ${pending.node}`);
      setPending(null);
      load();
    } catch (e) {
      setMsg(apiErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  function laneClass(l: Lane): string {
    const droppable = drag ? canMigrateTo(drag, l) : false;
    return 'topo-lane' + (l.ready === 'False' ? ' down' : '') + (over === l.node && droppable ? ' drop' : '') + (drag && !droppable ? ' dim' : '');
  }

  return (
    <div>
      {msg && <p className="msg error">{msg}</p>}
      {pending && (
        <div className="tile topo-confirm" role="alertdialog" aria-label="Confirm migration">
          <span>
            Migrate <b>{pending.machine.metadata.name}</b> from <b>{pending.machine.status?.nodeName || pending.machine.spec.nodeName}</b> to <b>{pending.node}</b>?
          </span>
          <span className="rowactions">
            <button className="primary" disabled={busy} onClick={confirmMove}>{busy ? 'Starting...' : 'Migrate'}</button>
            <button disabled={busy} onClick={() => setPending(null)}>Cancel</button>
          </span>
        </div>
      )}
      {lanes.length === 0 && !msg && <EmptyHint what="nodes or machines" namespaced={false} />}
      <div className="topo">
        {lanes.map((l) => (
          <section
            key={l.node || '_unscheduled'}
            className={laneClass(l)}
            aria-label={l.node || 'Unscheduled'}
            onDragOver={(e) => {
              if (drag && canMigrateTo(drag, l)) {
                e.preventDefault();
                setOver(l.node);
              }
            }}
            onDragLeave={() => setOver('')}
            onDrop={(e) => {
              e.preventDefault();
              if (drag && canMigrateTo(drag, l)) setPending({ machine: drag, node: l.node });
              setDrag(null);
              setOver('');
            }}
          >
            <header>
              <strong>{l.node || 'Unscheduled'}</strong>
              {l.node !== UNSCHEDULED && <span className={'badge ' + (l.ready === 'True' ? 'badge-ok' : 'badge-error')}>{l.ready === 'True' ? 'Ready' : 'Not ready'}</span>}
            </header>
            <div className="topo-items">
              {l.machines.map((m) => (
                <a
                  key={`${m.metadata.namespace}/${m.metadata.name}`}
                  className={'topo-vm ' + (TONE[m.status?.phase ?? 'Unknown'] ?? '')}
                  href={formatHash({ page: 'machines', target: `${m.metadata.namespace ?? ns}/${m.metadata.name}` })}
                  title={`${m.metadata.name} · ${m.status?.phase ?? 'Unknown'}${movable(m) ? ' · drag to migrate' : ''}`}
                  draggable={movable(m)}
                  onDragStart={() => setDrag(m)}
                  onDragEnd={() => {
                    setDrag(null);
                    setOver('');
                  }}
                >
                  <span className="topo-name">{m.metadata.name}</span>
                </a>
              ))}
              {l.pods.map((p) => (
                <span key={`${p.namespace}/${p.name}`} className={'topo-pod ' + (p.ready && p.phase === 'Running' ? 'ok' : 'warn')} title={`pod ${p.namespace}/${p.name} · ${p.phase}`} />
              ))}
              {l.machines.length === 0 && l.pods.length === 0 && <span className="usageHint">Empty</span>}
            </div>
          </section>
        ))}
      </div>
      <p className="usageHint">Circles are VMs, squares are pods. Drag a running VM onto another Ready node to stage a migration.</p>
    </div>
  );
}
