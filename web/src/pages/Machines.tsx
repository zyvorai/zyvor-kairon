// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { lazy, Suspense, useEffect, useState } from 'react';
import { api, apiJSON, getConfig, isAdmin } from '../api';
import { Machine } from '../types';
import { formatBytes } from '../lib/phase';
import { parseHash } from '../lib/route';
import { Plus } from 'lucide-react';
import ResourceTable, { Column } from '../components/ResourceTable';
import Inspector, { KV } from '../components/Inspector';
import MachineOps from '../components/MachineOps';
import Sheet from '../components/Sheet';
import { EmptyState, Status } from '../components/ui';
import { useToast } from '../components/Toast';
import Console from './Console';
import Exec from './Exec';
import AgentFiles from './AgentFiles';
import Logs from './Logs';
import NetworkPanel from './NetworkPanel';
import { currentNamespace } from '../lib/namespace';
// Lazy-loaded: @xterm/xterm alone adds ~300kB to the bundle, not worth
// shipping to every visitor when only a Machine with
// spec.guestAgent.console even shows this button.
const TextConsole = lazy(() => import('./TextConsole'));

// QEMU is the only backend FluxVM gives a VNC display to at all (Cloud
// Hypervisor/Firecracker have no display device) -- an empty/"auto"
// backend defaults to qemu server-side, so both count as eligible too.
function consoleEligible(m: Machine): boolean {
  const backend = m.spec.runtime?.backend;
  return m.status?.phase === 'Running' && (!backend || backend === 'qemu' || backend === 'auto');
}

// textConsoleEligible mirrors internal/uiapi/console.go's own
// spec.guestAgent.console check -- unlike consoleEligible above, this
// works on every backend, since FluxVM's own vsock console channel isn't
// tied to a QEMU-specific display device.
function textConsoleEligible(m: Machine): boolean {
  return m.status?.phase === 'Running' && !!m.spec.guestAgent?.console;
}

// execEligible mirrors internal/uiapi/exec.go's own server-side checks
// that don't depend on the caller's identity (Running + guestAgent
// enabled) -- the admin-only check is applied separately via isAdmin(),
// since that's a property of who's looking at this page, not of the
// Machine. Unlike the VNC console, exec works for every backend: it rides
// FluxVM's real qemu-guest-agent channel, not a QEMU-specific display
// device.
function execEligible(m: Machine): boolean {
  return m.status?.phase === 'Running' && !!m.spec.guestAgent?.enabled;
}

// logsEligible mirrors internal/uiapi/logs.go's own server-side check:
// a runtime just needs to have existed (Running or Paused), not
// currently Running specifically -- a Paused Machine's log file is still
// there on the node.
function logsEligible(m: Machine): boolean {
  return m.status?.phase === 'Running' || m.status?.phase === 'Paused';
}

interface CreateForm {
  name: string;
  image: string;
  cpu: string;
  memory: string;
  backend: string;
  network: string;
  netns: boolean;
  hostname: string;
  sshAuthorizedKey: string;
  forward: string;
}

const EMPTY_FORM: CreateForm = {
  name: '',
  image: '',
  cpu: '2',
  memory: '2Gi',
  backend: 'qemu',
  network: 'user',
  netns: false,
  hostname: '',
  sshAuthorizedKey: '',
  forward: '',
};

export default function Machines({ onMigrate, onSnapshot }: { onMigrate: (machine: string) => void; onSnapshot: (machine: string) => void }) {
  const [items, setItems] = useState<Machine[]>([]);
  const [form, setForm] = useState<CreateForm>(EMPTY_FORM);
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const [consoleFor, setConsoleFor] = useState<string | null>(null);
  const [textConsoleFor, setTextConsoleFor] = useState<string | null>(null);
  const [execFor, setExecFor] = useState<string | null>(null);
  const [agentFilesFor, setAgentFilesFor] = useState<string | null>(null);
  const [logsFor, setLogsFor] = useState<string | null>(null);
  const [networkFor, setNetworkFor] = useState<string | null>(null);
  // priorityEdits holds an in-progress, not-yet-saved priority value per
  // Machine name -- keyed separately from `items` so a value the operator
  // is mid-typing never gets clobbered by the 5s poll in `refresh` (see
  // the useEffect below), the same reason `form`/create isn't derived
  // from `items` either.
  const [priorityEdits, setPriorityEdits] = useState<Record<string, string>>({});
  // consoleEnabled also gates exec: both ride the exact same kairon-ui ->
  // kairon-node relay (internal/consoleproxy), so a deployment either has
  // that relay configured or it doesn't -- see internal/uiapi/exec.go's
  // own doc comment.
  const [consoleEnabled, setConsoleEnabled] = useState(false);
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<string | null>(() => {
    const t = parseHash(window.location.hash).target;
    return t ? t.split('/').pop() ?? null : null;
  });
  const toast = useToast();

  // Palette "New machine" and #/machines/<ns>/<name> deep links.
  useEffect(() => {
    const onNew = () => setCreating(true);
    const onHash = () => {
      const t = parseHash(window.location.hash).target;
      if (t) setSelected(t.split('/').pop() ?? null);
    };
    window.addEventListener('kairon:new-machine', onNew);
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('kairon:new-machine', onNew);
      window.removeEventListener('hashchange', onHash);
    };
  }, []);

  const refresh = () =>
    api<Machine[]>('/api/v1/machines').then(setItems).catch((e) => setMsg(String(e)));

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 5000);
    getConfig()
      .then((cfg) => setConsoleEnabled(cfg.consoleEnabled))
      .catch(() => setConsoleEnabled(false));
    return () => clearInterval(t);
  }, []);

  async function create(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setMsg('');
    try {
      const { hostname, sshAuthorizedKey, forward, ...base } = form;
      const body: Record<string, unknown> = { ...base };
      if (hostname) body.hostname = hostname;
      if (sshAuthorizedKey) body.sshAuthorizedKeys = [sshAuthorizedKey];
      if (forward) {
        const [hostPort, guestPort] = forward.split(':').map((p) => Number(p.trim()));
        if (!hostPort || !guestPort) throw new Error('Port forward must be hostPort:guestPort, e.g. 2222:22');
        body.forwards = [{ hostPort, guestPort }];
      }
      await apiJSON('/api/v1/machines', 'POST', body);
      setForm(EMPTY_FORM);
      setCreating(false);
      toast(`Machine ${form.name} created`);
      await refresh();
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function power(name: string, action: 'start' | 'stop' | 'pause' | 'resume' | 'halt') {
    // Stop and halt interrupt a running guest: ask first, like delete does.
    if (action === 'stop' && !confirm(`Stop machine "${name}"? The guest is shut down.`)) return;
    if (action === 'halt' && !confirm(`Halt machine "${name}"? The guest is powered off without a clean shutdown.`)) return;
    setMsg('');
    try {
      await api(`/api/v1/machines/${currentNamespace()}/${encodeURIComponent(name)}/${action}`, { method: 'POST' });
      const done = { start: 'Started', stop: 'Stopped', pause: 'Paused', resume: 'Resumed', halt: 'Halted' }[action];
      toast(`${done} ${name}`);
      await refresh();
    } catch (err) {
      setMsg(String(err));
      toast(`${action} ${name} failed`, 'err');
    }
  }

  // setPriority patches spec.priority via the same single-field pattern
  // `power` above uses for spec.powerState -- kairon-ui's own counterpart
  // to `kaironctl edit machine NAME --priority N` (see
  // docs/guides/machine-placement.md's "Scheduling priority" section),
  // which until now was kaironctl/kubectl-only.
  async function setPriority(name: string) {
    const raw = priorityEdits[name];
    const priority = Number(raw);
    if (raw === undefined || raw.trim() === '' || !Number.isInteger(priority)) {
      setMsg('Priority must be a whole number');
      return;
    }
    setMsg('');
    try {
      await apiJSON(`/api/v1/machines/${currentNamespace()}/${encodeURIComponent(name)}/priority`, 'POST', { priority });
      setPriorityEdits((prev) => {
        const next = { ...prev };
        delete next[name];
        return next;
      });
      await refresh();
    } catch (err) {
      setMsg(String(err));
    }
  }

  async function remove(name: string) {
    if (!confirm(`Delete machine "${name}"? This deletes the underlying VM runtime too.`)) return;
    setMsg('');
    try {
      await api(`/api/v1/machines/${currentNamespace()}/${encodeURIComponent(name)}`, { method: 'DELETE' });
      await refresh();
    } catch (err) {
      setMsg(String(err));
    }
  }

  const selectedMachine = items.find((m) => m.metadata.name === selected);
  const columns: Column<Machine>[] = [
    { header: 'Name', render: (m) => <b>{m.metadata.name}</b>, sortValue: (m) => m.metadata.name },
    { header: 'Node', render: (m) => m.spec.nodeName || m.status?.nodeName || '-', sortValue: (m) => m.spec.nodeName || m.status?.nodeName || '' },
    { header: 'Phase', render: (m) => <Status phase={m.status?.phase} />, sortValue: (m) => m.status?.phase || 'Unknown' },
    {
      header: 'CPU',
      render: (m) => (
        <>
          {m.spec.resources.cpu}
          {m.status?.resourceUsage?.cpuPercent !== undefined && <span className="usageHint"> ({m.status.resourceUsage.cpuPercent.toFixed(0)}%)</span>}
        </>
      ),
      csv: (m) => m.spec.resources.cpu,
    },
    {
      header: 'Memory',
      render: (m) => (
        <>
          {m.spec.resources.memory}
          {m.status?.resourceUsage?.memoryBytes !== undefined && <span className="usageHint"> ({formatBytes(m.status.resourceUsage.memoryBytes)})</span>}
        </>
      ),
      csv: (m) => m.spec.resources.memory,
    },
    { header: 'IP', render: (m) => m.status?.guestIP || '-', sortValue: (m) => m.status?.guestIP || '' },
    {
      header: '',
      render: (m) => (
        <div className="rowactions" onClick={(e) => e.stopPropagation()}>
          {m.status?.phase === 'Running' ? (
            <button className="sm" onClick={() => power(m.metadata.name, 'stop')}>Stop</button>
          ) : m.status?.phase === 'Paused' ? (
            <button className="sm" onClick={() => power(m.metadata.name, 'resume')}>Resume</button>
          ) : (
            <button className="sm" onClick={() => power(m.metadata.name, 'start')}>Start</button>
          )}
          <button className="sm ghost" onClick={() => setSelected(m.metadata.name)}>Details</button>
        </div>
      ),
    },
  ];

  const m = selectedMachine;
  return (
    <div>
      <div className="toolbar" style={{ justifyContent: 'space-between' }}>
        <span className="usageHint">{items.length} machine{items.length === 1 ? '' : 's'}</span>
        <button className="primary" onClick={() => setCreating(true)}>
          <Plus size={14} style={{ verticalAlign: -2 }} /> New machine
        </button>
      </div>
      {msg && <p className="msg error">{msg}</p>}
      {items.length === 0 && !msg ? (
        <div className="card">
          <EmptyState title="No machines yet">Create one to get started — pick an image path on the node and a size.</EmptyState>
        </div>
      ) : (
        <ResourceTable
          items={items}
          columns={columns}
          keyFn={(x) => x.metadata.name}
          emptyText="No machines yet."
          onRowClick={(x) => setSelected(x.metadata.name)}
          selectedKey={selected ?? undefined}
          csvName="kairon-machines"
        />
      )}

      {creating && (
        <Sheet title="New machine" onClose={() => setCreating(false)}>
          <form onSubmit={create}>
            <div className="formgrid">
              <label>
                Name
                <input required autoFocus value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
              </label>
              <label>
                Image path
                <input required value={form.image} onChange={(e) => setForm({ ...form, image: e.target.value })} placeholder="/var/lib/fluxvm/images/db.qcow2" />
              </label>
              <label>
                CPU
                <input value={form.cpu} onChange={(e) => setForm({ ...form, cpu: e.target.value })} />
              </label>
              <label>
                Memory
                <input value={form.memory} onChange={(e) => setForm({ ...form, memory: e.target.value })} />
              </label>
              <label>
                Backend
                <select value={form.backend} onChange={(e) => setForm({ ...form, backend: e.target.value })}>
                  <option value="qemu">qemu</option>
                  <option value="cloud-hypervisor">cloud-hypervisor</option>
                  <option value="firecracker">firecracker</option>
                  <option value="flux-vm">flux-vm</option>
                  <option value="auto">auto</option>
                </select>
              </label>
              <label>
                Network
                <select value={form.network} onChange={(e) => setForm({ ...form, network: e.target.value })}>
                  <option value="user">user</option>
                  <option value="tap">tap</option>
                  <option value="macvtap">macvtap</option>
                </select>
              </label>
              <div className="check">
                <input type="checkbox" id="netns" checked={form.netns} onChange={(e) => setForm({ ...form, netns: e.target.checked })} />
                <label htmlFor="netns">Per-VM network namespace</label>
              </div>
              <label>
                Port forward (host:guest)
                <input value={form.forward} onChange={(e) => setForm({ ...form, forward: e.target.value })} placeholder="2222:22 (network=user only)" />
              </label>
              <label>
                Hostname
                <input value={form.hostname} onChange={(e) => setForm({ ...form, hostname: e.target.value })} placeholder="set via cloud-init" />
              </label>
              <label className="full">
                SSH public key
                <input value={form.sshAuthorizedKey} onChange={(e) => setForm({ ...form, sshAuthorizedKey: e.target.value })} placeholder="ssh-ed25519 AAAA... (authorized via cloud-init)" />
              </label>
            </div>
            <div className="formactions">
              <button className="primary" type="submit" disabled={busy}>
                {busy ? 'Creating...' : 'Create machine'}
              </button>
              <button type="button" onClick={() => setCreating(false)}>Cancel</button>
              {msg && <span className="msg error">{msg}</span>}
            </div>
          </form>
        </Sheet>
      )}

      {m && (
        <Inspector
          title={m.metadata.name}
          subtitle={`${m.metadata.namespace} · ${m.spec.nodeName || m.status?.nodeName || 'unscheduled'}`}
          status={<Status phase={m.status?.phase} />}
          onClose={() => setSelected(null)}
          actions={
            <>
              {m.status?.phase === 'Running' && <button onClick={() => power(m.metadata.name, 'pause')}>Pause</button>}
              {m.status?.phase === 'Paused' && <button onClick={() => power(m.metadata.name, 'resume')}>Resume</button>}
              {m.status?.phase === 'Running' && <button onClick={() => power(m.metadata.name, 'halt')}>Halt</button>}
              {m.status?.phase === 'Halted' && <button onClick={() => power(m.metadata.name, 'start')}>Resume</button>}
              <button onClick={() => onMigrate(m.metadata.name)}>Migrate</button>
              <button onClick={() => onSnapshot(m.metadata.name)}>Snapshot</button>
              <button className="danger" onClick={() => remove(m.metadata.name)}>Delete</button>
            </>
          }
        >
          <h4>Overview</h4>
          <KV
            rows={[
              ['Phase', m.status?.phase || 'Unknown'],
              ['Guest IP', m.status?.guestIP],
              ['CPU', m.spec.resources.cpu],
              ['Memory', m.spec.resources.memory],
              ['Image', m.spec.image?.path],
              ['Backend', m.spec.runtime?.backend],
              ['Network', m.spec.network?.mode],
              ['Created', m.metadata.creationTimestamp],
            ]}
          />
          <h4>Scheduling priority</h4>
          <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0' }}>
            <input
              type="number"
              step={1}
              className="priorityInput"
              value={priorityEdits[m.metadata.name] ?? String(m.spec.priority ?? 0)}
              onChange={(e) => setPriorityEdits((prev) => ({ ...prev, [m.metadata.name]: e.target.value }))}
            />
            {priorityEdits[m.metadata.name] !== undefined && priorityEdits[m.metadata.name] !== String(m.spec.priority ?? 0) && (
              <button className="sm" onClick={() => setPriority(m.metadata.name)}>Set</button>
            )}
          </div>
          <h4>Tools</h4>
          <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0' }}>
            {consoleEnabled && consoleEligible(m) && <button onClick={() => setConsoleFor(m.metadata.name)}>Console</button>}
            {consoleEnabled && textConsoleEligible(m) && <button onClick={() => setTextConsoleFor(m.metadata.name)}>Text console</button>}
            {consoleEnabled && isAdmin() && execEligible(m) && <button onClick={() => setExecFor(m.metadata.name)}>Exec</button>}
            {consoleEnabled && isAdmin() && textConsoleEligible(m) && <button onClick={() => setAgentFilesFor(m.metadata.name)}>Files</button>}
            {consoleEnabled && logsEligible(m) && <button onClick={() => setLogsFor(m.metadata.name)}>Logs</button>}
            {consoleEnabled && logsEligible(m) && <button onClick={() => setNetworkFor(m.metadata.name)}>Network</button>}
            {!consoleEnabled && <span className="usageHint">Console relay is not enabled on this deployment.</span>}
          </div>
          {consoleEnabled && m.status?.phase && m.status.phase !== 'Pending' && (
            <MachineOps namespace={m.metadata.namespace || currentNamespace()} name={m.metadata.name} />
          )}
        </Inspector>
      )}

      {consoleFor && <Console namespace={currentNamespace()} name={consoleFor} onClose={() => setConsoleFor(null)} />}
      {textConsoleFor && (
        <Suspense fallback={<div className="consoleOverlay" />}>
          <TextConsole namespace={currentNamespace()} name={textConsoleFor} onClose={() => setTextConsoleFor(null)} />
        </Suspense>
      )}
      {execFor && <Exec namespace={currentNamespace()} name={execFor} onClose={() => setExecFor(null)} />}
      {agentFilesFor && <AgentFiles namespace={currentNamespace()} name={agentFilesFor} onClose={() => setAgentFilesFor(null)} />}
      {logsFor && <Logs namespace={currentNamespace()} name={logsFor} onClose={() => setLogsFor(null)} />}
      {networkFor && <NetworkPanel namespace={currentNamespace()} name={networkFor} onClose={() => setNetworkFor(null)} />}
    </div>
  );
}
