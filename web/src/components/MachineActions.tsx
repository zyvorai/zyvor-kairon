// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useState } from 'react';
import { apiErrorText, apiJSON } from '../api';
import { useToast } from './Toast';

// Fork, disk and NIC hotplug from the machine inspector. Each button maps
// onto POST /api/v1/machines/{ns}/{name}/{fork,disks,nics}; the server
// validates with the same model functions kaironctl uses and refuses
// anything the Machine's phase or network mode cannot take.
export default function MachineActions({
  namespace,
  name,
  running,
  onChanged,
}: {
  namespace: string;
  name: string;
  running: boolean;
  onChanged?: () => void;
}) {
  const base = `/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [forkCount, setForkCount] = useState('1');
  const [disk, setDisk] = useState({ name: '', claim: '' });
  const [nic, setNic] = useState({ name: '', bridge: '' });
  const [quiesce, setQuiesce] = useState('auto');

  async function act(label: string, fn: () => Promise<unknown>) {
    setBusy(true);
    setErr('');
    try {
      await fn();
      toast(label);
      onChanged?.();
    } catch (e) {
      setErr(apiErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <h4>Fork</h4>
      <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0' }}>
        <input aria-label="Number of copies" type="number" min={1} max={32} className="priorityInput" value={forkCount} onChange={(e) => setForkCount(e.target.value)} />
        <button
          disabled={busy || !running}
          title={running ? 'Fork this running Machine' : 'Only a Running Machine can be forked'}
          onClick={() => act(`Forked ${name}`, () => apiJSON(`${base}/fork`, 'POST', { count: Number(forkCount) || 1 }))}
        >
          Fork
        </button>
      </div>

      <h4>Backup</h4>
      <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0' }}>
        <select aria-label="Filesystem freeze" value={quiesce} onChange={(e) => setQuiesce(e.target.value)}>
          <option value="auto">Freeze if possible</option>
          <option value="required">Freeze required</option>
          <option value="never">No freeze</option>
        </select>
        <button disabled={busy} onClick={() => act(`Backup of ${name} started`, () => apiJSON(`${base}/backups`, 'POST', { quiesce }))}>Back up now</button>
      </div>

      <h4>Hot-attach disk</h4>
      <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0', flexWrap: 'wrap' }}>
        <input aria-label="Disk name" placeholder="disk name" value={disk.name} onChange={(e) => setDisk({ ...disk, name: e.target.value })} />
        <input aria-label="PVC claim" placeholder="PVC claim" value={disk.claim} onChange={(e) => setDisk({ ...disk, claim: e.target.value })} />
        <button disabled={busy || !disk.name || !disk.claim} onClick={() => act(`Disk ${disk.name} attaching`, () => apiJSON(`${base}/disks`, 'POST', { action: 'attach', name: disk.name, claim: disk.claim }))}>Attach</button>
        <button disabled={busy || !disk.name} onClick={() => act(`Disk ${disk.name} detaching`, () => apiJSON(`${base}/disks`, 'POST', { action: 'detach', name: disk.name }))}>Detach</button>
      </div>

      <h4>Hot-add NIC</h4>
      <div className="rowactions" style={{ justifyContent: 'flex-start', padding: '8px 0', flexWrap: 'wrap' }}>
        <input aria-label="Interface name" placeholder="interface name" value={nic.name} onChange={(e) => setNic({ ...nic, name: e.target.value })} />
        <input aria-label="Host bridge" placeholder="host bridge" value={nic.bridge} onChange={(e) => setNic({ ...nic, bridge: e.target.value })} />
        <button disabled={busy || !nic.name || !nic.bridge} onClick={() => act(`NIC ${nic.name} adding`, () => apiJSON(`${base}/nics`, 'POST', { action: 'add', name: nic.name, bridge: nic.bridge }))}>Add</button>
        <button disabled={busy || !nic.name} onClick={() => act(`NIC ${nic.name} removing`, () => apiJSON(`${base}/nics`, 'POST', { action: 'remove', name: nic.name }))}>Remove</button>
      </div>
      {err && <p className="msg error">{err}</p>}
    </>
  );
}
