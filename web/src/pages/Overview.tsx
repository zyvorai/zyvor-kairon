// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react';
import { api, downloadFile, apiErrorText } from '../api';
import { NodeUsage, Overview as OverviewData } from '../types';
import { formatBytes } from '../lib/phase';
import { Kpi, KpiStrip, Meter, Reveal, useSeries } from '../components/ui';
import { currentNamespace } from '../lib/namespace';
import InsightsStrip from '../components/InsightsStrip';

function phaseTone(phase: string): string {
  if (phase === 'Running' || phase === 'Succeeded') return 'ok';
  if (phase === 'Failed' || phase === 'Error' || phase === 'NeedsRecovery' || phase === 'Blocked') return 'bad';
  if (phase === 'Paused' || phase === 'Pending' || phase === 'Unknown') return 'warn';
  return '';
}

export default function Overview() {
  const [data, setData] = useState<OverviewData | null>(null);
  const [usage, setUsage] = useState<NodeUsage[]>([]);
  const [msg, setMsg] = useState('');

  useEffect(() => {
    let cancelled = false;
    const refresh = () => {
      api<OverviewData>('/api/v1/overview')
        .then((d) => !cancelled && setData(d))
        .catch((e) => !cancelled && setMsg(String(e)));
      api<NodeUsage[]>('/api/v1/nodes/usage')
        .then((u) => !cancelled && setUsage(u))
        .catch(() => {});
    };
    refresh();
    const t = setInterval(refresh, 5000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, []);

  const running = data?.machines.byPhase['Running'] ?? 0;
  const cpuAvg = usage.length ? usage.reduce((a, u) => a + u.cpuPercent, 0) / usage.length : undefined;
  const memTotal = usage.reduce((a, u) => a + u.memoryBytes, 0);
  const runningSeries = useSeries(data ? running : undefined);
  const machineSeries = useSeries(data?.machines.total);
  const cpuSeries = useSeries(cpuAvg);

  if (msg && !data) return <p className="msg error">{msg}</p>;
  if (!data) return <p className="msg">Loading...</p>;

  const total = data.machines.total || 1;
  const phases = Object.entries(data.machines.byPhase).sort((a, b) => b[1] - a[1]);

  return (
    <div>
      <div className="rowactions" style={{ justifyContent: 'flex-end', marginBottom: 8 }}>
        <button
          className="sm"
          onClick={() => downloadFile(`/api/v1/usage.csv?namespace=${encodeURIComponent(currentNamespace())}`, 'kairon-usage.csv').catch((e) => alert(apiErrorText(e)))}
        >
          Download usage CSV
        </button>
      </div>
      <InsightsStrip />
      <KpiStrip>
        <Kpi label="Machines" value={data.machines.total} sub={`${running} running`} series={machineSeries} />
        <Kpi label="Running" value={running} sub={data.machines.total ? `${Math.round((running / data.machines.total) * 100)}% of fleet` : 'none yet'} series={runningSeries} />
        <Kpi label="Nodes" value={data.nodes} sub={`${memTotal ? formatBytes(memTotal) + ' in use' : 'no usage data'}`} />
        <Kpi
          label="Migrations"
          value={data.migrations.active}
          sub={`${data.migrations.total} total`}
          bad={data.migrations.needsRecovery > 0}
        />
        <Kpi label="Avg node CPU" value={cpuAvg === undefined ? '-' : `${cpuAvg.toFixed(0)}%`} sub="across reporting nodes" series={cpuSeries} />
      </KpiStrip>

      {data.migrations.needsRecovery > 0 && (
        <div className="tile warning-tile" style={{ marginBottom: 16 }}>
          <b>{data.migrations.needsRecovery}</b>
          <span>
            migration{data.migrations.needsRecovery === 1 ? '' : 's'} NeedsRecovery -- stopped by design, waiting on an operator. See Lifecycle &rsaquo; Migrations.
          </span>
        </div>
      )}

      <Reveal>
        <div className="grid">
          <div className="card span2" data-tone="violet">
            <span className="eyebrow">Machines by phase</span>
            <div className="fleet-hero" style={{ marginTop: 14 }}>
              <div className="phase-bars">
                {phases.length === 0 && <p>No machines yet.</p>}
                {phases.map(([phase, count]) => (
                  <div className="phase-bar" key={phase}>
                    <span>{phase}</span>
                    <span className="track">
                      <i className={phaseTone(phase)} style={{ width: `${(count / total) * 100}%` }} />
                    </span>
                    <b>{count}</b>
                  </div>
                ))}
              </div>
            </div>
          </div>

          <div className="card span2" data-tone="sky">
            <span className="eyebrow">Node usage</span>
            <div className="phase-bars" style={{ marginTop: 14 }}>
              {usage.length === 0 && <p>No node usage reported yet.</p>}
              {usage.map((u) => (
                <div className="phase-bar" key={u.node} style={{ gridTemplateColumns: '1fr 90px 90px' }}>
                  <span>
                    <b>{u.node}</b> <span className="usageHint">{u.machines} machine{u.machines === 1 ? '' : 's'}</span>
                  </span>
                  <span>
                    <Meter value={u.cpuPercent} /> <span className="usageHint">{u.cpuPercent.toFixed(0)}%</span>
                  </span>
                  <span className="usageHint">{formatBytes(u.memoryBytes)}</span>
                </div>
              ))}
            </div>
          </div>
        </div>
      </Reveal>
    </div>
  );
}
