// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { formatMiB } from '../lib/capacity';
import { NodeTopologyResponse, numaRows } from '../lib/numaTopology';

// NUMA nodes, their devices and the Machines placed on them, from the
// node's NodeTopology and each Machine's spec.topologyAllocation.
export default function NodeTopologyPanel({ node }: { node: string }) {
  const [data, setData] = useState<NodeTopologyResponse>();
  const [err, setErr] = useState('');

  useEffect(() => {
    setData(undefined);
    setErr('');
    api<NodeTopologyResponse>(`/api/v1/nodes/${encodeURIComponent(node)}/topology`)
      .then(setData)
      .catch((e) => setErr(apiErrorText(e)));
  }, [node]);

  if (err) return <p className="msg error">{err}</p>;
  if (!data) return null;
  if (!data.topology) return <p className="usageHint">No NodeTopology yet: kairon-node publishes it once FluxVM reports host topology.</p>;
  return (
    <div className="topology">
      <span className="eyebrow">TOPOLOGY {data.topology.numaBinding ? '' : '(no NUMA binding)'}</span>
      {numaRows(data).map((row) => (
        <div key={row.id} className="topologyNuma">
          <strong>{row.id < 0 ? 'No NUMA node' : `numa${row.id}`}</strong>
          {row.id >= 0 && (
            <span className="usageHint">
              {' '}
              cpus {row.cpus} · {formatMiB(row.memoryMiB)}
            </span>
          )}
          <ul>
            {row.devices.map((d) => (
              <li key={d.bdf}>
                {d.class} <code>{d.bdf}</code> {d.vendor}:{d.device}
                {d.vramGiB ? ` ${d.vramGiB}GiB` : ''}
                {d.owner ? ` → ${d.owner}` : d.allocated ? ' (in use outside Kairon)' : d.allocatable ? '' : ' (not in VFIO allowlist)'}
              </li>
            ))}
            {row.machines.map((m) => (
              <li key={m.ref + m.cpus}>
                {m.ref} cpus {m.cpus} · {formatMiB(m.memoryMiB)}
                {m.warning && <span className="badge badge-warn" title={m.warning}> drift</span>}
              </li>
            ))}
          </ul>
        </div>
      ))}
    </div>
  );
}
