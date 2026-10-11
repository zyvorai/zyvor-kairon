// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, apiErrorText } from '../api';
import { linePath, Point } from '../lib/chart';
import { formatBytes } from '../lib/phase';

interface Sample {
  time: string;
  cpuPercent: number;
  memoryBytes: number;
  diskReadBytes: number;
  diskWriteBytes: number;
}

function Spark({ label, value, points, tone }: { label: string; value: string; points: Point[]; tone: string }) {
  const d = linePath(points, 280, 44);
  return (
    <figure className="metric">
      <figcaption>
        <span>{label}</span>
        <strong>{value}</strong>
      </figcaption>
      <svg viewBox="0 0 280 44" role="img" aria-label={`${label} over the last 15 minutes: ${value}`} preserveAspectRatio="none">
        {d && <path d={d} fill="none" stroke={tone} strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />}
      </svg>
    </figure>
  );
}

// Recent CPU/memory/disk history for one Machine, from the in-memory
// sampler behind GET /api/v1/machines/{ns}/{name}/metrics.
export default function MachineMetrics({ namespace, name }: { namespace: string; name: string }) {
  const [samples, setSamples] = useState<Sample[] | null>(null);
  const [err, setErr] = useState('');

  useEffect(() => {
    let live = true;
    const load = () =>
      api<{ samples: Sample[] }>(`/api/v1/machines/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/metrics?range=15m`)
        .then((r) => live && (setSamples(r.samples ?? []), setErr('')))
        .catch((e) => live && setErr(apiErrorText(e)));
    load();
    const t = setInterval(load, 15000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [namespace, name]);

  const series = (pick: (s: Sample) => number): Point[] => (samples ?? []).map((s) => ({ t: Date.parse(s.time), v: pick(s) }));
  const last = samples && samples.length > 0 ? samples[samples.length - 1] : undefined;

  return (
    <>
      <h4>Usage</h4>
      {err && <p className="msg error">{err}</p>}
      {!err && samples !== null && samples.length === 0 && (
        <p className="usageHint">No usage samples yet. History builds up while the dashboard server runs.</p>
      )}
      {last && (
        <div className="metrics">
          <Spark label="CPU" value={`${last.cpuPercent.toFixed(0)}%`} points={series((s) => s.cpuPercent)} tone="var(--tone-sky)" />
          <Spark label="Memory" value={formatBytes(last.memoryBytes)} points={series((s) => s.memoryBytes)} tone="var(--tone-violet)" />
          <Spark label="Disk read" value={formatBytes(last.diskReadBytes)} points={series((s) => s.diskReadBytes)} tone="var(--tone-emerald)" />
          <Spark label="Disk write" value={formatBytes(last.diskWriteBytes)} points={series((s) => s.diskWriteBytes)} tone="var(--tone-amber)" />
        </div>
      )}
    </>
  );
}
