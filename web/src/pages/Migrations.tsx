// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiJSON } from '../api';
import { MachineMigration } from '../types';
import { useLiveChanges } from '../lib/live';
import { badgeClass, formatBytes, formatDuration, transferProgress } from '../lib/phase';
import TerminalFrame from '../components/TerminalFrame';
import { allNamespaces, currentNamespace } from '../lib/namespace';

interface CreateForm {
  machine: string;
  strategy: string;
  targetNode: string;
  mode: string;
  bandwidthMbps: string;
  maxDowntimeMs: string;
  multifdChannels: string;
  migrationNetwork: string;
}

const EMPTY_FORM: CreateForm = { machine: '', strategy: 'auto', targetNode: '', mode: 'pre-copy', bandwidthMbps: '', maxDowntimeMs: '', multifdChannels: '', migrationNetwork: '' };

export default function Migrations({ prefillMachine }: { prefillMachine: string }) {
  const [items, setItems] = useState<MachineMigration[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [form, setForm] = useState<CreateForm>({ ...EMPTY_FORM, machine: prefillMachine });
  const [evacuateNode, setEvacuateNode] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (prefillMachine) setForm((f) => ({ ...f, machine: prefillMachine }));
  }, [prefillMachine]);

  const refresh = () =>
    api<MachineMigration[]>('/api/v1/migrations').then((r) => setItems(r ?? [])).catch((e) => setMsg(String(e)));

  // Stream-driven: a migration change refetches immediately; polling is
  // only the backstop (RAM-transfer progress isn't a watch event until the
  // controller writes status, so keep a short poll while disconnected).
  const live = useLiveChanges((c) => {
    if (c.kind === 'MachineMigration') refresh();
  });
  useEffect(() => {
    refresh();
    const t = setInterval(refresh, live ? 10000 : 3000);
    return () => clearInterval(t);
  }, [live]);

  const showNs = allNamespaces();
  const keyOf = (m: MachineMigration) => `${m.metadata.namespace || ''}/${m.metadata.name}`;
  const current = items.find((m) => keyOf(m) === selected) || null;

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMsg('');
    try {
      const body: Record<string, unknown> = { machine: form.machine, strategy: form.strategy, targetNode: form.targetNode, mode: form.mode, migrationNetwork: form.migrationNetwork };
      if (form.bandwidthMbps) body.bandwidthMbps = Number(form.bandwidthMbps);
      if (form.maxDowntimeMs) body.maxDowntimeMs = Number(form.maxDowntimeMs);
      if (form.multifdChannels) body.multifdChannels = Number(form.multifdChannels);
      await apiJSON('/api/v1/migrations', 'POST', body);
      setForm(EMPTY_FORM);
      await refresh();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function evacuate() {
    if (!evacuateNode) return;
    if (!confirm(`Evacuate every machine currently on node "${evacuateNode}"? This creates one migration per machine.`)) return;
    setMsg('');
    try {
      const out = await apiJSON<{ created: string[] }>('/api/v1/migrations/evacuate', 'POST', { node: evacuateNode, strategy: 'cold' });
      setMsg(`Queued ${out.created.length} migration(s) from ${evacuateNode}.`);
      await refresh();
    } catch (err) {
      setMsg(String(err));
    }
  }

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">NEW MIGRATION</span>
        <h3>Migrate a machine</h3>
        <form onSubmit={create}>
          <div className="formgrid">
            <label>
              Machine
              <input required value={form.machine} onChange={(e) => setForm({ ...form, machine: e.target.value })} />
            </label>
            <label>
              Strategy
              <select value={form.strategy} onChange={(e) => setForm({ ...form, strategy: e.target.value })}>
                <option value="auto">auto</option>
                <option value="live">live</option>
                <option value="cold">cold</option>
              </select>
            </label>
            <label>
              Target node
              <input value={form.targetNode} onChange={(e) => setForm({ ...form, targetNode: e.target.value })} placeholder="empty lets the scheduler choose" />
            </label>
            <label>
              Mode
              <select value={form.mode} onChange={(e) => setForm({ ...form, mode: e.target.value })}>
                <option value="pre-copy">pre-copy</option>
                <option value="post-copy">post-copy</option>
              </select>
            </label>
            <label>
              Bandwidth (Mbps)
              <input value={form.bandwidthMbps} onChange={(e) => setForm({ ...form, bandwidthMbps: e.target.value })} placeholder="0 = unlimited" />
            </label>
            <label>
              Max downtime (ms)
              <input value={form.maxDowntimeMs} onChange={(e) => setForm({ ...form, maxDowntimeMs: e.target.value })} />
            </label>
            <label>
              Migration network
              <input value={form.migrationNetwork} onChange={(e) => setForm({ ...form, migrationNetwork: e.target.value })} placeholder="empty uses the adapter default" />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Submitting...' : 'Start migration'}
            </button>
          </div>
        </form>

        <h3 style={{ marginTop: 28 }}>Evacuate a node</h3>
        <div className="toolbar">
          <input value={evacuateNode} onChange={(e) => setEvacuateNode(e.target.value)} placeholder="node name" />
          <button onClick={evacuate}>Evacuate (cold)</button>
        </div>
        {msg && <p className={msg.toLowerCase().includes('error') ? 'msg error' : 'msg'}>{msg}</p>}
      </div>

      <div className="card span4">
        <span className="eyebrow">MIGRATIONS</span>
        <table className="datatable">
          <thead>
            <tr>
              {showNs && <th>Namespace</th>}
              <th>Name</th>
              <th>Machine</th>
              <th>Strategy</th>
              <th>Source</th>
              <th>Target</th>
              <th>Phase</th>
            </tr>
          </thead>
          <tbody>
            {items.map((m) => (
              <tr key={keyOf(m)} onClick={() => setSelected(keyOf(m))} style={{ cursor: 'pointer', background: selected === keyOf(m) ? 'var(--row-sel)' : undefined }}>
                {showNs && <td>{m.metadata.namespace}</td>}
                <td>{m.metadata.name}</td>
                <td>{m.spec.machineName}</td>
                <td>{m.status?.effectiveStrategy || m.spec.strategy || '-'}</td>
                <td>{m.status?.sourceNode || '-'}</td>
                <td>{m.status?.targetNode || m.spec.targetNode || '-'}</td>
                <td>
                  <span className={badgeClass(m.status?.phase || '')}>{m.status?.phase || 'Pending'}</span>
                </td>
              </tr>
            ))}
            {items.length === 0 && (
              <tr>
                <td colSpan={showNs ? 7 : 6} className="msg">
                  No migrations yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {current && <MigrationDetail migration={current} onChanged={refresh} />}
    </div>
  );
}

function MigrationDetail({ migration, onChanged }: { migration: MachineMigration; onChanged: () => void }) {
  const st = migration.status || {};
  const pct = transferProgress(st.ramTransferred, st.ramTotal);
  const cancellable = (st.phase === 'Starting' || st.phase === 'Running') && st.effectiveStrategy === 'live' && !migration.spec.cancel;
  return (
    <div className="card span4">
      <span className="eyebrow">{migration.metadata.name}</span>
      <h3>
        {migration.spec.machineName} <span className={badgeClass(st.phase || '')}>{st.phase || 'Pending'}</span>
      </h3>
      {st.message && <p>{st.message}</p>}
      <div className="grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))' }}>
        <div className="tile">
          <b>{formatBytes(st.ramTransferred)}</b>
          <span>RAM transferred</span>
        </div>
        <div className="tile">
          <b>{formatBytes(st.ramRemaining)}</b>
          <span>RAM remaining</span>
        </div>
        <div className="tile">
          <b>{formatBytes(st.ramTotal)}</b>
          <span>RAM total{pct !== null ? ` (${pct}%)` : ''}</span>
        </div>
        <div className="tile">
          <b>{formatDuration(st.totalTimeMs)}</b>
          <span>Total time</span>
        </div>
        <div className="tile">
          <b>{formatDuration(st.downtimeMs)}</b>
          <span>Downtime</span>
        </div>
        <div className={`tile ${st.dataPlaneEncrypted ? '' : 'warning-tile'}`}>
          <b>{st.dataPlaneEncrypted ? 'Encrypted' : 'Cleartext'}</b>
          <span>Data-plane transport</span>
        </div>
      </div>

      {cancellable && <CancelButton migration={migration} onChanged={onChanged} />}
      {migration.spec.cancel && (st.phase === 'Starting' || st.phase === 'Running') && (
        <p className="msg">Cancel requested -- waiting for the source node's agent to abort the transfer.</p>
      )}
      {st.phase === 'NeedsRecovery' && <RecoveryPanel migration={migration} onChanged={onChanged} />}
    </div>
  );
}

function CancelButton({ migration, onChanged }: { migration: MachineMigration; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState('');

  async function cancel() {
    if (
      !confirm(
        'Cancel this in-flight live migration?\n\nThe source and destination transfer will be aborted before the destination commits. The source runtime is left untouched and keeps running on its current node. This cannot be undone.',
      )
    )
      return;
    setBusy(true);
    setMsg('');
    try {
      await apiJSON(`/api/v1/migrations/${migration.metadata.namespace || currentNamespace()}/${encodeURIComponent(migration.metadata.name)}/cancel`, 'POST', {});
      onChanged();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="formactions" style={{ marginTop: 16 }}>
      <button className="danger" disabled={busy} onClick={cancel}>
        {busy ? 'Cancelling...' : 'Cancel migration'}
      </button>
      {msg && <span className="msg error">{msg}</span>}
    </div>
  );
}

const ACTIONS = ['ConfirmDestinationCommitted', 'ConfirmDestinationNotCommitted', 'ForceAbort'];
const DIAGNOSES = ['DestinationCommitted', 'DestinationNotCommitted', 'Unknown'];

function RecoveryPanel({ migration, onChanged }: { migration: MachineMigration; onChanged: () => void }) {
  const [action, setAction] = useState('');
  const [diagnosis, setDiagnosis] = useState('');
  const [reason, setReason] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const r = migration.status?.recovery;

  async function submit() {
    if (!action || !diagnosis || !reason) return;
    const consequence =
      action === 'ForceAbort'
        ? 'the destination session will be aborted and this migration will be marked Failed; the SOURCE runtime is deliberately left untouched'
        : action === 'ConfirmDestinationCommitted'
          ? 'Kairon will treat the destination as the live copy and proceed with cutover; do this only if you have confirmed the destination guest is actually running'
          : 'Kairon will retry the commit and, if it fails again, mark this migration Failed without deleting the source runtime';
    if (!confirm(`Apply recovery action "${action}"?\n\n${consequence}\n\nThis cannot be undone.`)) return;
    setBusy(true);
    setMsg('');
    try {
      await apiJSON(`/api/v1/migrations/${migration.metadata.namespace || currentNamespace()}/${encodeURIComponent(migration.metadata.name)}/recover`, 'POST', {
        action,
        acknowledgedDiagnosis: diagnosis,
        reason,
      });
      setMsg('Recovery requested -- the source node agent will validate and apply it on its next reconcile.');
      onChanged();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="recovery-block">
      <h3>NeedsRecovery -- operator action required</h3>
      <p>
        The source transfer completed but the destination commit was ambiguous. Kairon has deliberately stopped reconciling this migration until you attest what actually happened -- see the current diagnosis
        below before choosing an action.
      </p>
      <TerminalFrame title="status.recovery (live diagnosis)">
        <div className="recovery-fields">
          <div>
            <span>Source runtime status</span>
            <b>{r?.sourceRuntimeStatus || '-'}</b>
          </div>
          <div>
            <span>Destination session phase</span>
            <b>{r?.destinationSessionPhase || '-'}</b>
          </div>
          <div>
            <span>Destination runtime found</span>
            <b>{r?.destinationRuntimeFound ? 'true' : 'false'}</b>
          </div>
          <div>
            <span>Destination runtime status</span>
            <b>{r?.destinationRuntimeStatus || '-'}</b>
          </div>
          <div>
            <span>Diagnosed at</span>
            <b>{r?.diagnosedAt || '-'}</b>
          </div>
        </div>
      </TerminalFrame>

      <div className="formgrid" style={{ marginTop: 16 }}>
        <label>
          Action
          <select value={action} onChange={(e) => setAction(e.target.value)}>
            <option value="">select...</option>
            {ACTIONS.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </label>
        <label>
          Acknowledged diagnosis
          <select value={diagnosis} onChange={(e) => setDiagnosis(e.target.value)}>
            <option value="">select...</option>
            {DIAGNOSES.map((d) => (
              <option key={d} value={d}>
                {d}
              </option>
            ))}
          </select>
        </label>
        <label className="full">
          Reason (what you observed -- required)
          <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. confirmed via FluxVM status that the destination guest is running" />
        </label>
      </div>
      <div className="formactions">
        <button className="danger" disabled={!action || !diagnosis || !reason || busy} onClick={submit}>
          {busy ? 'Applying...' : 'Apply recovery action'}
        </button>
        {msg && <span className="msg">{msg}</span>}
      </div>
    </div>
  );
}
