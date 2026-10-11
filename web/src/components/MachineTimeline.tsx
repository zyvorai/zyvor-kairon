// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { relativeTime, TimelineEntry, timelineTone } from '../lib/timeline';

// Chronological Events + condition transitions for one Machine, from
// GET /api/v1/machines/{ns}/{name}/timeline. Refetches when the machine's
// phase changes so a transition shows up without reopening the inspector.
export default function MachineTimeline({ namespace, name, phase }: { namespace: string; name: string; phase?: string }) {
  const [rows, setRows] = useState<TimelineEntry[] | null>(null);
  const [err, setErr] = useState('');

  useEffect(() => {
    let live = true;
    api<TimelineEntry[]>(`/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/timeline`)
      .then((r) => {
        if (!live) return;
        setRows(r ?? []);
        setErr('');
      })
      .catch((e) => live && setErr(apiErrorText(e)));
    return () => {
      live = false;
    };
  }, [namespace, name, phase]);

  return (
    <>
      <h4>Timeline</h4>
      {err && <p className="msg error">{err}</p>}
      {!err && rows === null && <p className="usageHint">Loading…</p>}
      {!err && rows !== null && rows.length === 0 && <p className="usageHint">No events recorded yet.</p>}
      {rows && rows.length > 0 && (
        <ol className="timeline" aria-label="Machine timeline">
          {rows.map((e, i) => (
            <li key={`${e.time}-${e.source}-${e.type}-${i}`} className={`timeline-row tone-${timelineTone(e)}`}>
              <span className="timeline-dot" aria-hidden="true" />
              <div className="timeline-main">
                <div className="timeline-title">
                  <strong>{e.reason || e.type}</strong>
                  {e.source === 'condition' && <span className="timeline-tag">{e.type}={e.status}</span>}
                  {e.count && e.count > 1 ? <span className="timeline-tag">×{e.count}</span> : null}
                  <time dateTime={e.time} title={e.time}>{relativeTime(e.time)}</time>
                </div>
                {e.message && <div className="timeline-msg">{e.message}</div>}
              </div>
            </li>
          ))}
        </ol>
      )}
    </>
  );
}
