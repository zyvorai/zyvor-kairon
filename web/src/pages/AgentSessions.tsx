// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText, apiJSON, isAdmin } from '../api';
import { badgeClass } from '../lib/phase';
import { currentNamespace } from '../lib/namespace';
import ResourceTable from '../components/ResourceTable';
import EmptyHint from '../components/EmptyHint';

interface Session {
  metadata: { name: string; namespace?: string };
  spec: { agent?: string; nodeName?: string; workspace?: { git?: string }; policy?: { preset?: string }; ttlSeconds?: number };
  status?: {
    phase?: string;
    message?: string;
    sessionID?: string;
    backend?: string;
    expiresAt?: string;
    checkpoints?: { tag: string; memory: boolean }[];
    usage?: { execs?: number; jobs?: number; children?: number };
    policy?: { unenforced?: string[]; offline?: boolean; allowHosts?: string[] };
  };
}
interface Step {
  seq: number;
  ts: string;
  source: string;
  kind: string;
  detail?: Record<string, unknown>;
}
interface Preview {
  token: string;
  port: number;
  url?: string;
  served?: boolean;
  expires_at?: string;
}

interface Approval {
  id: string;
  kind?: string;
  summary?: string;
  matched?: string;
  state: string;
  requested_at?: string;
  expires_at?: string;
  decided_by?: string;
  note?: string;
}

// approvalIDOf returns the approval id of a 428 from exec (a command matched
// policy.process.ask), or '' for any other error.
export function approvalIDOf(e: unknown): string {
  try {
    const doc = JSON.parse(e instanceof Error ? e.message : String(e));
    return doc?.approval_required && typeof doc.approval_id === 'string' ? doc.approval_id : '';
  } catch {
    return '';
  }
}

const verbPath = (name: string, verb: string) =>
  `/api/v1/agentsessions/${encodeURIComponent(currentNamespace())}/${encodeURIComponent(name)}/${verb}`;

function until(ts?: string): string {
  if (!ts) return '-';
  const s = Math.round((new Date(ts).getTime() - Date.now()) / 1000);
  if (s <= 0) return 'expired';
  if (s < 120) return `${s}s`;
  if (s < 7200) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

// AgentSessions lists FluxVM agent sessions and drives the selected one:
// exec, checkpoint, previews and its trajectory. Verbs need an admin, or an
// operator when kairon-ui allows sandbox exec.
export default function AgentSessions() {
  const [items, setItems] = useState<Session[]>([]);
  const [msg, setMsg] = useState('');
  const [form, setForm] = useState({ name: '', agent: '', git: '', preset: '', ttl: '60' });
  const [busy, setBusy] = useState(false);
  const [selected, setSelected] = useState('');

  const refresh = () =>
    api<Session[]>('/api/v1/agentsessions').then((r) => setItems(r ?? [])).catch((e) => setMsg(apiErrorText(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMsg('');
    const spec: Record<string, unknown> = {};
    if (form.agent) spec.agent = form.agent;
    if (form.git) spec.workspace = { git: form.git };
    if (form.preset) spec.policy = { preset: form.preset };
    if (Number(form.ttl) > 0) spec.ttlSeconds = Number(form.ttl) * 60;
    try {
      const s = await apiJSON<Session>('/api/v1/agentsessions', 'POST', { name: form.name, spec });
      setForm({ ...form, name: '' });
      setSelected(s.metadata.name);
      await refresh();
    } catch (err) {
      setMsg(apiErrorText(err));
    } finally {
      setBusy(false);
    }
  }

  const remove = async (name: string) => {
    if (!confirm(`Delete AgentSession "${name}"? Its VM, workspace and checkpoints are deleted.`)) return;
    try {
      await api(`/api/v1/agentsessions/${encodeURIComponent(currentNamespace())}/${encodeURIComponent(name)}`, { method: 'DELETE' });
      if (selected === name) setSelected('');
      refresh();
    } catch (e) {
      setMsg(apiErrorText(e));
    }
  };

  const current = items.find((s) => s.metadata.name === selected);

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">NEW SESSION</span>
        <h3>Start a FluxVM agent session</h3>
        <p className="muted">A sandbox VM with a workspace, policy, checkpoints and an event ledger. Placed on a node that serves sessions (a Mac on FluxVM 0.4.8+).</p>
        <form onSubmit={create}>
          <div className="formgrid">
            <label>
              Name
              <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="generated if empty" />
            </label>
            <label>
              Agent
              <input value={form.agent} onChange={(e) => setForm({ ...form, agent: e.target.value })} placeholder="claude, codex, ..." />
            </label>
            <label>
              Git repository
              <input value={form.git} onChange={(e) => setForm({ ...form, git: e.target.value })} placeholder="https://github.com/org/repo" />
            </label>
            <label>
              Policy preset
              <select value={form.preset} onChange={(e) => setForm({ ...form, preset: e.target.value })}>
                <option value="">FluxVM default</option>
                <option value="offline">offline</option>
                <option value="developer-safe">developer-safe</option>
                <option value="open">open</option>
              </select>
            </label>
            <label>
              TTL (minutes)
              <input value={form.ttl} onChange={(e) => setForm({ ...form, ttl: e.target.value })} inputMode="numeric" />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Creating...' : 'Create session'}
            </button>
            {msg && <span className="msg error">{msg}</span>}
          </div>
        </form>
      </div>

      <div className="card span4">
        <span className="eyebrow">AGENT SESSIONS</span>
        <ResourceTable
          items={items}
          keyFn={(s) => s.metadata.name}
          emptyText={<EmptyHint what="agent sessions" hint="Create one above, or: kaironctl session run NAME --agent codex." />}
          columns={[
            { header: 'Name', render: (s) => s.metadata.name },
            { header: 'Phase', render: (s) => <span className={badgeClass(s.status?.phase || 'Pending')}>{s.status?.phase || 'Pending'}</span> },
            { header: 'Node', render: (s) => s.spec.nodeName || '-' },
            { header: 'Agent', render: (s) => s.spec.agent || '-' },
            { header: 'Policy', render: (s) => s.spec.policy?.preset || '-' },
            { header: 'Checkpoints', render: (s) => s.status?.checkpoints?.length ?? 0 },
            { header: 'Execs', render: (s) => s.status?.usage?.execs ?? 0 },
            { header: 'Expires', render: (s) => until(s.status?.expiresAt) },
            { header: 'Message', render: (s) => s.status?.message || '' },
            {
              header: '',
              render: (s) => (
                <span className="rowactions">
                  <button onClick={() => setSelected(s.metadata.name)}>Open</button>
                  <button className="danger" onClick={() => remove(s.metadata.name)}>
                    Delete
                  </button>
                </span>
              ),
            },
          ]}
        />
      </div>

      {current && <SessionPanel key={current.metadata.name} session={current} />}
    </div>
  );
}

function SessionPanel({ session }: { session: Session }) {
  const name = session.metadata.name;
  const running = session.status?.phase === 'Running';
  const [command, setCommand] = useState('');
  const [result, setResult] = useState<{ exit_code?: number; stdout?: string; stderr?: string } | null>(null);
  const [steps, setSteps] = useState<Step[]>([]);
  const [previews, setPreviews] = useState<Preview[]>([]);
  const [port, setPort] = useState('');
  const [tag, setTag] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const [approvals, setApprovals] = useState<Approval[]>([]);
  const [waiting, setWaiting] = useState<{ id: string; command: string } | null>(null);
  const admin = isAdmin();

  const loadApprovals = () =>
    running
      ? api<{ items?: Approval[] }>(verbPath(name, 'approvals'))
          .then((r) => setApprovals((r?.items ?? []).filter((a) => a.state === 'pending')))
          .catch(() => setApprovals([]))
      : undefined;
  const loadSteps = () =>
    api<{ steps?: Step[] }>(verbPath(name, 'trajectory'))
      .then((r) => setSteps((r?.steps ?? []).slice(-50).reverse()))
      .catch(() => setSteps([]));
  const loadPreviews = () =>
    running ? api<{ items?: Preview[] }>(verbPath(name, 'previews')).then((r) => setPreviews(r?.items ?? [])).catch(() => setPreviews([])) : undefined;

  useEffect(() => {
    if (session.status?.sessionID) loadSteps();
    loadPreviews();
    loadApprovals();
    if (!running) return;
    const t = setInterval(loadApprovals, 5000);
    return () => clearInterval(t);
  }, [name, session.status?.sessionID, running]);

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg('');
    try {
      await fn();
    } catch (e) {
      setMsg(apiErrorText(e));
    } finally {
      setBusy(false);
      loadSteps();
    }
  };

  const runCommand = (cmd: string, approvalID?: string) =>
    run(async () => {
      try {
        setResult(await apiJSON(verbPath(name, 'exec'), 'POST', approvalID ? { command: cmd, approval_id: approvalID } : { command: cmd }));
        setWaiting(null);
      } catch (err) {
        const id = approvalIDOf(err);
        if (!id) throw err;
        setWaiting({ id, command: cmd });
        loadApprovals();
      }
    });

  const exec = (e: React.FormEvent) => {
    e.preventDefault();
    if (command) runCommand(command);
  };

  const decide = (id: string, decision: 'approve' | 'deny') =>
    run(async () => {
      await apiJSON(verbPath(name, `approvals/${encodeURIComponent(id)}/${decision}`), 'POST', {});
      await loadApprovals();
    });

  const unenforced = session.status?.policy?.unenforced ?? [];

  return (
    <div className="card span4">
      <span className="eyebrow">SESSION {name.toUpperCase()}</span>
      <p className="muted">
        FluxVM session {session.status?.sessionID || 'not created yet'}
        {session.status?.backend ? ` on ${session.status.backend}` : ''}
        {session.spec.workspace?.git ? `, workspace from ${session.spec.workspace.git}` : ''}.
      </p>
      {unenforced.length > 0 && <p className="msg">Accepted but not enforced by FluxVM: {unenforced.join(', ')}</p>}
      {msg && <p className="msg error">{msg}</p>}

      <form onSubmit={exec}>
        <div className="formgrid">
          <label>
            Command
            <input value={command} onChange={(e) => setCommand(e.target.value)} placeholder="cd /workspace && make test" disabled={!running} />
          </label>
        </div>
        <div className="formactions">
          <button className="primary" type="submit" disabled={!running || busy}>
            Run
          </button>
          <input value={tag} onChange={(e) => setTag(e.target.value)} placeholder="checkpoint tag" disabled={!running} />
          <button type="button" disabled={!running || busy} onClick={() => run(() => apiJSON(verbPath(name, 'checkpoint'), 'POST', tag ? { tag } : {}))}>
            Checkpoint
          </button>
        </div>
      </form>
      {waiting && (
        <p className="msg">
          This command needs an admin's approval ({waiting.id}).{' '}
          <button type="button" disabled={busy} onClick={() => runCommand(waiting.command, waiting.id)}>
            Run approved
          </button>
        </p>
      )}
      {result && (
        <>
          <p className="muted">exit {result.exit_code ?? '-'}</p>
          {result.stdout && <pre className="execStream">{result.stdout}</pre>}
          {result.stderr && <pre className="execStream">{result.stderr}</pre>}
        </>
      )}

      {approvals.length > 0 && (
        <>
          <h4>Pending approvals</h4>
          <ResourceTable
            items={approvals}
            keyFn={(a) => a.id}
            emptyText="None."
            columns={[
              { header: 'Command', render: (a) => a.summary || a.kind || '-' },
              { header: 'Rule', render: (a) => a.matched || '-' },
              { header: 'Expires', render: (a) => until(a.expires_at) },
              {
                header: '',
                render: (a) =>
                  admin ? (
                    <span className="rowactions">
                      <button disabled={busy} onClick={() => decide(a.id, 'approve')}>
                        Approve
                      </button>
                      <button className="danger" disabled={busy} onClick={() => decide(a.id, 'deny')}>
                        Deny
                      </button>
                    </span>
                  ) : (
                    'an admin decides'
                  ),
              },
            ]}
          />
        </>
      )}

      <h4>Previews</h4>
      <div className="formactions">
        <input value={port} onChange={(e) => setPort(e.target.value)} placeholder="guest port" inputMode="numeric" disabled={!running} />
        <button
          type="button"
          disabled={!running || busy || !Number(port)}
          onClick={() => run(async () => { await apiJSON(verbPath(name, 'preview'), 'POST', { port: Number(port) }); await loadPreviews(); })}
        >
          Expose
        </button>
      </div>
      <ul>
        {previews.map((p) => (
          <li key={p.token}>
            port {p.port}: {p.served && p.url ? <a href={p.url} target="_blank" rel="noreferrer noopener">open</a> : 'not served by kairon-ui (ui.agentSessions.previews is off)'}
            {p.expires_at ? `, until ${new Date(p.expires_at).toLocaleTimeString()}` : ''}
          </li>
        ))}
      </ul>

      <h4>Trajectory</h4>
      <ResourceTable
        items={steps}
        keyFn={(s) => String(s.seq)}
        emptyText="No steps yet."
        columns={[
          { header: '#', render: (s) => s.seq },
          { header: 'Time', render: (s) => new Date(s.ts).toLocaleTimeString() },
          { header: 'Source', render: (s) => s.source },
          { header: 'Kind', render: (s) => s.kind },
          {
            header: 'Detail',
            render: (s) => {
              const d = s.detail ?? {};
              const bits = ['command', 'exit_code', 'duration_ms', 'tag', 'reason'].filter((k) => d[k] !== undefined).map((k) => `${k}=${String(d[k])}`);
              return bits.join(' ') || '-';
            },
          },
        ]}
      />
    </div>
  );
}
