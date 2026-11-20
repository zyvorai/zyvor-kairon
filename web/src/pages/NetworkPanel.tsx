// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useState } from 'react';
import { X } from 'lucide-react';
import { api } from '../api';

type Tab = 'status' | 'flows' | 'drops' | 'attributed' | 'effective';

function pretty(data: unknown): string {
  try {
    return JSON.stringify(data, null, 2);
  } catch {
    return String(data);
  }
}

// NetworkPanel surfaces FluxVM eBPF dataplane observability for one
// Machine -- the dashboard counterpart to `kaironctl network
// flows|drop-reasons|stats|effective`. Same uiapi relay as diagnostics
// (console token required).
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
  const [body, setBody] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      setBusy(true);
      setErr('');
      setBody('');
      const base = `/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
      try {
        let path = `${base}/network-stats`;
        if (tab === 'flows') path = `${base}/network-flows?limit=25`;
        else if (tab === 'drops') path = `${base}/network-drop-reasons?limit=25`;
        else if (tab === 'attributed') path = `${base}/network-drops?limit=25`;
        else if (tab === 'effective') path = `${base}/network-effective`;
        const data = await api<unknown>(path);
        if (!cancelled) setBody(pretty(data));
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
      ) : (
        <pre className="execStream" style={{ flex: 1, margin: 0, borderRadius: 0, maxHeight: 'none', overflow: 'auto' }}>
          {busy ? 'Loading…' : body || '(empty)'}
        </pre>
      )}
    </div>
  );
}
