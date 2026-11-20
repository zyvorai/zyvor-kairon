// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react';
import { apiErrorText, apiJSON } from '../api';
import JsonTerminal from './JsonTerminal';

// The deterministic agent-plane routes (internal/agentplane): pure functions
// over the JSON you post, no cluster writes and no model in the loop.
const TOOLS: { id: string; label: string; hint: string; sample: unknown }[] = [
  { id: 'compile-policy', label: 'Compile network policy', hint: 'Intent → MachineNetworkPolicy (never applied)', sample: { name: 'web', allowDNS: ['example.com'] } },
  { id: 'explain-drops', label: 'Explain drops', hint: 'Ranked hypotheses for eBPF edge drops', sample: { machine: 'web-1', drops: [] } },
  { id: 'anomalies', label: 'Flow anomalies', hint: 'Findings over flows + drops', sample: { machine: 'web-1', flows: [], drops: [] } },
  { id: 'cpu-label', label: 'CPU label', hint: 'Node CPU report → label update', sample: {} },
  { id: 'confidential', label: 'Confidential projection', hint: 'Requested class vs node attestation', sample: { requested: 'sev-snp', node: {} } },
  { id: 'gateway', label: 'Gateway binding', hint: 'Port forwards → binding', sample: { name: 'gw', forwards: [] } },
  { id: 'claims/step', label: 'Claim step', hint: 'One MachineClaim decision', sample: { claim: {}, warm: [] } },
];

export default function AgentTools() {
  const [tool, setTool] = useState(TOOLS[0].id);
  const [body, setBody] = useState(JSON.stringify(TOOLS[0].sample, null, 2));
  const [out, setOut] = useState<{ value?: unknown; error?: string }>({});
  const [busy, setBusy] = useState(false);
  const cur = TOOLS.find((t) => t.id === tool)!;

  async function run() {
    setBusy(true);
    try {
      setOut({ value: await apiJSON(`/api/v1/agent/${tool}`, 'POST', JSON.parse(body)) });
    } catch (e) {
      setOut({ error: e instanceof SyntaxError ? 'Body must be valid JSON.' : apiErrorText(e) });
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="card span4">
      <span className="eyebrow">AGENT TOOLS</span>
      <p className="usageHint">{cur.hint}. Results are advisory; nothing here is applied to the cluster.</p>
      <div className="opform">
        <label className="opfield">
          <span>Tool</span>
          <select
            value={tool}
            onChange={(e) => {
              setTool(e.target.value);
              setBody(JSON.stringify(TOOLS.find((t) => t.id === e.target.value)!.sample, null, 2));
              setOut({});
            }}
          >
            {TOOLS.map((t) => (
              <option key={t.id} value={t.id}>{t.label}</option>
            ))}
          </select>
        </label>
        <button className="primary" disabled={busy} onClick={run}>{busy ? 'Running…' : 'Run'}</button>
      </div>
      <textarea rows={8} spellCheck={false} value={body} onChange={(e) => setBody(e.target.value)} style={{ width: '100%', fontFamily: 'var(--font-mono)', fontSize: 12 }} />
      <JsonTerminal title={cur.id} value={out.value} error={out.error} />
    </div>
  );
}
