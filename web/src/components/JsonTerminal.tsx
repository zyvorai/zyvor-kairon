// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { ReactNode } from 'react';

// Black Terminal.app-style pane for raw API results (zorvia's AppleTerminalFrame).
export default function JsonTerminal({ title, value, error }: { title: string; value?: unknown; error?: string }) {
  if (value === undefined && !error) return null;
  const text = error ?? (typeof value === 'string' ? value : JSON.stringify(value, null, 2));
  return (
    <div className="terminal" style={{ marginTop: 10 }}>
      <div className="terminalbar">
        <span>
          <i />
          <i />
          <i />
        </span>
        <span>{title}</span>
      </div>
      <div className="terminalbody">
        <pre style={error ? { color: '#ff6b60' } : undefined}>{text}</pre>
      </div>
    </div>
  );
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="opfield">
      <span>{label}</span>
      {children}
    </label>
  );
}
