// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react';
import { api, apiErrorText, apiJSON, downloadFile, isAdmin } from '../api';
import JsonTerminal, { Field } from './JsonTerminal';

interface DiagBundle {
  token: string;
  state: string;
  error?: string;
  size_bytes?: number;
}

interface Capture {
  token: string;
  seconds?: number;
  filter?: string;
  state?: string;
}

// Diagnostics and consistency operations from the machine inspector. Every
// button maps onto an existing /api/v1/machines/{ns}/{name}/... route; the
// server enforces the real gates (admin, namespace, guest agent, phase).
export default function MachineOps({ namespace, name }: { namespace: string; name: string }) {
  const base = `/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
  const admin = isAdmin();
  const [out, setOut] = useState<{ title: string; value?: unknown; error?: string }>();
  const [busy, setBusy] = useState('');
  const [tag, setTag] = useState('');
  const [fw, setFw] = useState({ name: '', port: '', protocol: 'tcp' });
  const [cmd, setCmd] = useState('');
  const [cap, setCap] = useState({ seconds: '30', filter: '' });
  const [captures, setCaptures] = useState<Capture[]>([]);
  const [diag, setDiag] = useState({ pcapSeconds: '0', redact: true });
  const [bundle, setBundle] = useState<DiagBundle>();

  async function run(title: string, fn: () => Promise<unknown>) {
    setBusy(title);
    try {
      setOut({ title, value: await fn() });
    } catch (e) {
      setOut({ title, error: apiErrorText(e) });
    } finally {
      setBusy('');
    }
  }
  const get = (title: string, path: string) => run(title, () => api(`${base}/${path}`));
  const post = (title: string, path: string, body: unknown = {}) => run(title, () => apiJSON(`${base}/${path}`, 'POST', body));

  async function listCaptures() {
    await run('capture sessions', async () => {
      const list = await api<Capture[] | { items?: Capture[] }>(`${base}/network-capture`);
      const items = Array.isArray(list) ? list : list.items ?? [];
      setCaptures(items);
      return list;
    });
  }

  async function collectBundle() {
    await run('incident bundle', async () => {
      let job = await apiJSON<DiagBundle>(`${base}/diagnostics`, 'POST', {
        redact: diag.redact,
        pcap_seconds: Number(diag.pcapSeconds) || undefined,
      });
      setBundle(job);
      for (let i = 0; job.state === 'running' && i < 300; i++) {
        await new Promise((r) => setTimeout(r, 2000));
        job = await api<DiagBundle>(`${base}/diagnostics/${encodeURIComponent(job.token)}`);
        setBundle(job);
      }
      return job;
    });
  }

  return (
    <>
      <h4>Health</h4>
      <div className="rowactions opgrid">
        <button disabled={!!busy} onClick={() => get('pressure (PSI)', 'pressure')}>Pressure</button>
        <button disabled={!!busy} onClick={() => get('cpuset', 'cpuset')}>CPU set</button>
        <button disabled={!!busy} onClick={() => get('frozen state', 'frozen')}>Frozen?</button>
      </div>

      {admin && (
        <>
          <h4>Consistency</h4>
          <div className="rowactions opgrid">
            <button disabled={!!busy} onClick={() => post('freeze', 'freeze')}>Freeze</button>
            <button disabled={!!busy} onClick={() => post('thaw', 'thaw')}>Thaw</button>
            <button disabled={!!busy} onClick={() => get('guest fsfreeze', 'qga/fsfreeze-status')}>FS freeze status</button>
          </div>
          <div className="opform">
            <Field label="VM snapshot tag">
              <input value={tag} onChange={(e) => setTag(e.target.value)} placeholder="before-upgrade" />
            </Field>
            <button disabled={!!busy || !tag} onClick={() => post('vm-snapshot', 'vm-snapshot', { tag })}>Snapshot</button>
            <button
              className="danger"
              disabled={!!busy || !tag}
              onClick={() => confirm(`Restore "${tag}"? This stops and restarts the VM in place.`) && post('vm-restore-snapshot', 'vm-restore-snapshot', { tag })}
            >
              Restore
            </button>
          </div>

          <h4>Guest agent command</h4>
          <div className="opform">
            <Field label="Command (via the fluxvm guest agent, no SSH)">
              <input value={cmd} onChange={(e) => setCmd(e.target.value)} placeholder="uname -a" />
            </Field>
            <button disabled={!!busy || !cmd} onClick={() => post('agent-exec', 'agent-exec', { command: cmd, timeoutSeconds: 30 })}>
              Run
            </button>
          </div>

          <h4>Incident bundle</h4>
          <div className="opform">
            <Field label="Packet capture seconds (0 for none)">
              <input type="number" value={diag.pcapSeconds} onChange={(e) => setDiag({ ...diag, pcapSeconds: e.target.value })} />
            </Field>
            <label>
              <input type="checkbox" checked={diag.redact} onChange={(e) => setDiag({ ...diag, redact: e.target.checked })} /> Redact secrets
            </label>
            <button disabled={!!busy} onClick={collectBundle}>Collect</button>
            {bundle?.state === 'done' && (
              <button
                className="sm"
                onClick={() =>
                  downloadFile(`${base}/diagnostics/${encodeURIComponent(bundle.token)}/bundle`, `${name}-incident.tar.gz`).catch((e) =>
                    setOut({ title: 'download', error: apiErrorText(e) }),
                  )
                }
              >
                Download .tar.gz
              </button>
            )}
          </div>

          <h4>Guest firewall</h4>
          <div className="opform">
            <Field label="Rule name">
              <input value={fw.name} onChange={(e) => setFw({ ...fw, name: e.target.value })} placeholder="ssh" />
            </Field>
            <Field label="Port">
              <input type="number" value={fw.port} onChange={(e) => setFw({ ...fw, port: e.target.value })} placeholder="22" />
            </Field>
            <Field label="Protocol">
              <select value={fw.protocol} onChange={(e) => setFw({ ...fw, protocol: e.target.value })}>
                <option>tcp</option>
                <option>udp</option>
              </select>
            </Field>
            <button disabled={!!busy || !fw.name || !fw.port} onClick={() => post('firewall open', 'qga/firewall/open', { name: fw.name, port: Number(fw.port), protocol: fw.protocol })}>
              Open
            </button>
            <button disabled={!!busy || !fw.name} onClick={() => post('firewall close', 'qga/firewall/close', { name: fw.name })}>
              Close
            </button>
          </div>
        </>
      )}

      <h4>Packet capture</h4>
      <div className="opform">
        <Field label="Seconds">
          <input type="number" value={cap.seconds} onChange={(e) => setCap({ ...cap, seconds: e.target.value })} />
        </Field>
        <Field label="BPF filter (optional)">
          <input value={cap.filter} onChange={(e) => setCap({ ...cap, filter: e.target.value })} placeholder="tcp port 443" />
        </Field>
        <button
          disabled={!!busy}
          onClick={() =>
            post('capture started', 'network-capture', {
              token: crypto.randomUUID(),
              namespace,
              machine: name,
              seconds: Number(cap.seconds) || 30,
              filter: cap.filter || undefined,
              expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(),
            })
          }
        >
          Start
        </button>
        <button disabled={!!busy} onClick={listCaptures}>List</button>
      </div>
      {captures.length > 0 && (
        <ul className="oplist">
          {captures.map((c) => (
            <li key={c.token}>
              <code>{c.token.slice(0, 8)}</code> {c.state ?? ''}
              <button
                className="sm"
                onClick={() => downloadFile(`${base}/network-capture/${encodeURIComponent(c.token)}`, `${c.token}.pcap`).catch((e) => setOut({ title: 'download', error: apiErrorText(e) }))}
              >
                Download .pcap
              </button>
            </li>
          ))}
        </ul>
      )}
      {busy && <p className="usageHint">Running {busy}…</p>}
      <JsonTerminal title={out?.title ?? ''} value={out?.value} error={out?.error} />
    </>
  );
}
