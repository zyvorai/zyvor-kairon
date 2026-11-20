// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { X } from 'lucide-react';
import { ReactNode, useEffect } from 'react';

export function KV({ rows }: { rows: [string, ReactNode][] }) {
  return (
    <dl style={{ margin: 0 }}>
      {rows
        .filter(([, v]) => v !== undefined && v !== null && v !== '')
        .map(([k, v]) => (
          <div className="kv" key={k}>
            <dt>{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
    </dl>
  );
}

// Right-hand slide-over: details for the selected row, plus actions.
export default function Inspector({
  title,
  subtitle,
  status,
  onClose,
  actions,
  children,
}: {
  title: string;
  subtitle?: ReactNode;
  status?: ReactNode;
  onClose: () => void;
  actions?: ReactNode;
  children: ReactNode;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);
  return (
    <>
      <div className="inspector-scrim" onClick={onClose} />
      <aside className="inspector" aria-label={`${title} details`}>
        <div className="inspector-head">
          <div style={{ flex: 1, minWidth: 0 }}>
            <h2>{title}</h2>
            <div className="sub">{subtitle}</div>
            {status && <div style={{ marginTop: 8 }}>{status}</div>}
          </div>
          <button className="icon ghost" onClick={onClose} aria-label="Close inspector">
            <X size={16} />
          </button>
        </div>
        <div className="inspector-body">{children}</div>
        {actions && <div className="inspector-actions">{actions}</div>}
      </aside>
    </>
  );
}
