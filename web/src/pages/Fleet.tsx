// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { currentNamespace } from '../lib/namespace';
import { useEffect, useState } from 'react';
import { api, apiJSON, isAdmin } from '../api';
import { badgeClass } from '../lib/phase';
import { factsOf, FleetStatus, isStale } from '../lib/fleet';

const RESOURCES = [
  ['machineautoscalers', 'Autoscalers'], ['machinebalancepolicies', 'Balancing'],
  ['machinebackupgroups', 'Backup groups'], ['machinerecoveryplans', 'Recovery plans'],
  ['machineimportplans', 'Import campaigns'], ['machinetemplateversions', 'Templates'],
  ['machinetemplateclaims', 'Template claims'], ['machinevirtualnetworks', 'Virtual networks'],
  ['machinenetworkclaims', 'Address claims'], ['machineusageledgers', 'Usage ledgers'],
  ['machinehaprofiles', 'HA profiles'], ['nodefencerequests', 'Fence requests'],
];
interface FleetObject { kind: string; metadata: { name: string; namespace: string }; spec: unknown; status?: FleetStatus }

export default function Fleet() {
  const [namespace, setNamespace] = useState(currentNamespace());
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [resource, setResource] = useState('machineautoscalers');
  const [items, setItems] = useState<FleetObject[]>([]);
  const [error, setError] = useState('');
  const [request, setRequest] = useState('');
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);
  useEffect(() => { let active = true; api<string[]>('/api/v1/namespaces').then(ns => { if (!active) return; ns = ns ?? []; setNamespaces(ns); if (ns.length) setNamespace(ns[0]); }).catch(e => { if (active) setError(String(e)); }); return () => { active = false; }; }, []);
  useEffect(() => {
    let active = true; setError(''); setItems([]);
    api<FleetObject[]>(`/api/v1/fleet/${resource}?namespace=${encodeURIComponent(namespace)}`).then(rows => { if (active) setItems(rows ?? []); }).catch(e => { if (active) setError(String(e)); });
    return () => { active = false; };
  }, [namespace, resource, revision]);
  async function create() {
    setBusy(true); setError('');
    try { await apiJSON(`/api/v1/fleet/${resource}?namespace=${encodeURIComponent(namespace)}`, 'POST', JSON.parse(request)); setRequest(''); setRevision(r => r + 1); }
    catch (e) { setError(String(e)); } finally { setBusy(false); }
  }
  return <section className="panel">
    <h2>Fleet operations</h2>
    <p>Manage scaling, balancing, backups, recovery and VM templates. Plans show their current state and any blocked prerequisite.</p>
    <div className="toolbar">
      <label>Namespace <select value={namespace} onChange={e => setNamespace(e.target.value)}>{namespaces.map(ns => <option key={ns}>{ns}</option>)}</select></label>
      <label>Resource <select value={resource} onChange={e => setResource(e.target.value)}>{RESOURCES.filter(([r]) => isAdmin() || !['machinehaprofiles', 'nodefencerequests'].includes(r)).map(([r, label]) => <option value={r} key={r}>{label}</option>)}</select></label>
      <button onClick={() => setRevision(r => r + 1)}>Refresh</button>
    </div>
    {error && <p role="alert" className="error">{error}</p>}
    <div className="fleetgrid">{items.map(o => {
      const phase = o.status?.phase || 'Pending';
      const facts = factsOf(o.status);
      return <article className="healthcard fleetcard" key={`${o.metadata.namespace}/${o.metadata.name}`}>
        <header><strong>{o.metadata.name}</strong><span className={badgeClass(phase)}>{phase}</span></header>
        {o.status?.message && <p className="fleetmsg">{o.status.message}</p>}
        {isStale(o.status) && <p className="msg error">Telemetry is stale; decisions may be on old data.</p>}
        {facts.length > 0 && <dl className="fleetfacts">{facts.map(f => <div key={f.label}><dt>{f.label}</dt><dd>{f.value}</dd></div>)}</dl>}
      </article>;
    })}</div>
    {!items.length && !error && <p>No resources in this namespace.</p>}
    {(isAdmin() || ['machinetemplateclaims', 'machinenetworkclaims'].includes(resource)) && <details><summary>Create a resource</summary><p>Paste a resource from the documented examples. Changes take effect when the controller processes them.</p><textarea aria-label="Resource JSON" rows={10} value={request} onChange={e => setRequest(e.target.value)} /><button disabled={busy || !request.trim()} onClick={create}>{busy ? 'Creating…' : 'Create'}</button></details>}
  </section>;
}
