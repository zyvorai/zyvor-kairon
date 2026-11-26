// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useState, type ReactNode } from 'react';
import { X } from 'lucide-react';
import { api } from '../api';
import { DropRow, FlowRow, parseDrops, parseFlows, parseStats, StatsSummary } from '../lib/network';

type Tab = 'status' | 'flows' | 'drops' | 'attributed' | 'effective';

function pretty(data: unknown): string {
  try {
    return JSON.stringify(data, null, 2);
  } catch {
    return String(data);
  }
}

function Table<T extends object>({ columns, rows }: { columns: { key: keyof T & string; label: string }[]; rows: T[] }) {
  return (
    <div className="table-wrap" style={{ flex: 1, overflow: 'auto', padding: 16 }}>
      <table className="datatable">
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key}>{c.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}>
              {columns.map((c) => (
                <td key={c.key}>{String(row[c.key] ?? '-')}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function flowColumns(rows: FlowRow[]): { key: keyof FlowRow & string; label: string }[] {
  const cols: { key: keyof FlowRow & string; label: string }[] = [
    { key: 'proto', label: 'Proto' },
    { key: 'src', label: 'Src' },
    { key: 'dst', label: 'Dst' },
    { key: 'packets', label: 'Packets' },
    { key: 'bytes', label: 'Bytes' },
  ];
  if (rows.some((r) => r.rtt !== '-')) cols.push({ key: 'rtt', label: 'RTT' });
  if (rows.some((r) => r.process !== '-')) cols.push({ key: 'process', label: 'Process' });
  if (rows.some((r) => r.hostProcess !== '-')) cols.push({ key: 'hostProcess', label: 'Host' });
  if (rows.some((r) => r.reason !== '-')) cols.push({ key: 'reason', label: 'Reason' });
  return cols;
}

function dropColumns(rows: DropRow[]): { key: keyof DropRow & string; label: string }[] {
  const cols: { key: keyof DropRow & string; label: string }[] = [
    { key: 'reason', label: 'Reason' },
    { key: 'policy', label: 'Policy' },
    { key: 'src', label: 'Src' },
    { key: 'dst', label: 'Dst' },
    { key: 'packets', label: 'Packets' },
  ];
  if (rows.some((r) => r.process !== '-')) cols.push({ key: 'process', label: 'Process' });
  if (rows.some((r) => r.rtt !== '-')) cols.push({ key: 'rtt', label: 'RTT' });
  return cols;
}

function StatsCard({ s }: { s: StatsSummary }) {
  return (
    <div className="healthgrid" style={{ padding: 16 }}>
      <div className="healthcard">
        <strong>{s.rxPackets}</strong>
        <span className="healthcount">rx packets</span>
      </div>
      <div className="healthcard">
        <strong>{s.txPackets}</strong>
        <span className="healthcount">tx packets</span>
      </div>
      <div className="healthcard">
        <strong>{s.rxBytes}</strong>
        <span className="healthcount">rx bytes</span>
      </div>
      <div className="healthcard">
        <strong>{s.txBytes}</strong>
        <span className="healthcount">tx bytes</span>
      </div>
      <div className="healthcard">
        <strong>{s.dropped}</strong>
        <span className="healthcount">dropped</span>
      </div>
      {s.flows ? (
        <div className="healthcard">
          <strong>{s.flows}</strong>
          <span className="healthcount">flows</span>
        </div>
      ) : null}
      {s.rtt !== '-' ? (
        <div className="healthcard">
          <strong>{s.rtt}</strong>
          <span className="healthcount">{s.minRtt !== '-' ? `rtt (min ${s.minRtt})` : 'rtt'}</span>
        </div>
      ) : null}
    </div>
  );
}

// NetworkPanel surfaces FluxVM eBPF dataplane observability for one
// Machine -- the dashboard counterpart to `kaironctl network
// flows|drop-reasons|stats|effective`. Same uiapi relay as diagnostics
// (console token required). Process and RTT columns appear when FluxVM
// or PacketWolf supplies them on the payload.
export default function NetworkPanel({
  namespace,
  name,
  onClose,
}: {
  namespace: string;
  name: string;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>('status');
  const [data, setData] = useState<unknown>(null);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      setBusy(true);
      setErr('');
      setData(null);
      const base = `/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
      try {
        let path = `${base}/network-stats`;
        if (tab === 'flows') path = `${base}/network-flows?limit=25`;
        else if (tab === 'drops') path = `${base}/network-drop-reasons?limit=25`;
        else if (tab === 'attributed') path = `${base}/network-drops?limit=25`;
        else if (tab === 'effective') path = `${base}/network-effective`;
        const body = await api<unknown>(path);
        if (!cancelled) setData(body);
      } catch (e) {
        if (!cancelled) setErr(String(e));
      } finally {
        if (!cancelled) setBusy(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [namespace, name, tab]);

  let table: ReactNode = null;
  if (!busy && !err && data != null) {
    if (tab === 'status') {
      const stats = parseStats(data);
      if (stats) table = <StatsCard s={stats} />;
    } else if (tab === 'flows') {
      const rows = parseFlows(data);
      if (rows && rows.length === 0) table = <p style={{ padding: 16 }}>No flows.</p>;
      else if (rows) table = <Table columns={flowColumns(rows)} rows={rows} />;
    } else if (tab === 'attributed') {
      const rows = parseDrops(data);
      if (rows && rows.length === 0) table = <p style={{ padding: 16 }}>No drops.</p>;
      else if (rows) table = <Table columns={dropColumns(rows)} rows={rows} />;
    }
  }

  return (
    <div className="consoleOverlay">
      <div className="consoleBar">
        <span>
          Network &middot; <strong>{name}</strong>
        </span>
        <div className="rowactions" style={{ marginLeft: 'auto' }}>
          {(
            [
              ['status', 'Stats'],
              ['flows', 'Flows'],
              ['drops', 'Drops'],
              ['attributed', 'Attributed'],
              ['effective', 'Effective'],
            ] as const
          ).map(([id, label]) => (
            <button key={id} className={tab === id ? 'primary' : undefined} onClick={() => setTab(id)} disabled={busy}>
              {label}
            </button>
          ))}
        </div>
        <button className="linklike" onClick={onClose} aria-label="Close network panel">
          <X size={20} />
        </button>
      </div>
      {err ? (
        <pre className="execStream" style={{ flex: 1, margin: 0, color: 'var(--danger, #c44)' }}>
          {err}
        </pre>
      ) : busy ? (
        <pre className="execStream" style={{ flex: 1, margin: 0, borderRadius: 0, maxHeight: 'none', overflow: 'auto' }}>
          Loading…
        </pre>
      ) : (
        <>
          {table}
          <pre className="execStream" style={{ flex: table ? '0 0 auto' : 1, margin: 0, borderRadius: 0, maxHeight: table ? 240 : 'none', overflow: 'auto' }}>
            {pretty(data) || '(empty)'}
          </pre>
        </>
      )}
    </div>
  );
}
