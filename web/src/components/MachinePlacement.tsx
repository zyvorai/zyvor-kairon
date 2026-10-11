// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { NodeCapacity } from '../lib/capacity';

// "Where would this fit?" for one Machine: runs the scheduler's capacity
// filter for its requested CPU and memory against every node. It explains
// capacity only; taints, affinity and quota are decided by the controller
// and appear in the timeline.
export default function MachinePlacement({ cpu, memory }: { cpu: string; memory: string }) {
  const [rows, setRows] = useState<NodeCapacity[] | null>(null);
  const [err, setErr] = useState('');

  useEffect(() => {
    let live = true;
    api<NodeCapacity[]>(`/api/v1/nodes/capacity?cpu=${encodeURIComponent(cpu)}&memory=${encodeURIComponent(memory)}`)
      .then((r) => live && (setRows(r ?? []), setErr('')))
      .catch((e) => live && setErr(apiErrorText(e)));
    return () => {
      live = false;
    };
  }, [cpu, memory]);

  return (
    <>
      <h4>Placement</h4>
      {err && <p className="usageHint">Node capacity is not available to this account.</p>}
      {!err && rows === null && <p className="usageHint">Checking nodes…</p>}
      {rows && rows.length === 0 && <p className="usageHint">No nodes reported.</p>}
      {rows && rows.length > 0 && (
        <ul className="placement">
          {rows.map((n) => (
            <li key={n.node}>
              <span className={'badge ' + (n.whatIf?.fits ? 'badge-ok' : 'badge-error')}>{n.whatIf?.fits ? 'Fits' : 'No'}</span>
              <strong>{n.node}</strong>
              {!n.whatIf?.fits && n.whatIf?.reason && <span className="usageHint">{n.whatIf.reason}</span>}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
