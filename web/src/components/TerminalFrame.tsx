// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

export default function TerminalFrame({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="terminal">
      <div className="terminalbar">
        <div>
          <i />
          <i />
          <i />
        </div>
        <span>{title}</span>
      </div>
      <div className="terminalbody">{children}</div>
    </section>
  );
}
