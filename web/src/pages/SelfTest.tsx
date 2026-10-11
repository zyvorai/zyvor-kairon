// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useRef, useState } from 'react';
import { api, apiErrorText, apiJSON, isAdmin } from '../api';
import {
  formatAt, formatDuration, GLYPH, Line, mergeLines, Run, scenariosFor, Scenario, stepGlyph, verdict,
} from '../lib/selftest';

type Profile = 'quick' | 'standard' | 'full';

const PROFILE_BLURB: Record<Profile, string> = {
  quick: 'Control plane, boot, console log, power. About a minute.',
  standard: 'Adds network, pause/resume, CPU hotplug and observability.',
  full: 'Adds fork and cold migration, plus NIC/disk hotplug when configured.',
};

// SelfTest boots a CirrOS Machine from the server and shows the same
// coloured log as `kaironctl selftest`, in a terminal-style panel that stays
// dark in both themes. It polls GET /api/v1/selftest/{id}?after=N.
export default function SelfTest() {
  const [catalogue, setCatalogue] = useState<Scenario[]>([]);
  const [profile, setProfile] = useState<Profile>('standard');
  const [noVm, setNoVm] = useState(false);
  const [run, setRun] = useState<Run | null>(null);
  const [lines, setLines] = useState<Line[]>([]);
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const after = useRef(0);
  const body = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const admin = isAdmin();

  useEffect(() => {
    api<Scenario[]>('/api/v1/selftest/scenarios').then((r) => setCatalogue(r ?? [])).catch(() => {});
  }, []);

  // Re-attach to a run that is still going after a page reload.
  useEffect(() => {
    if (!admin) return;
    api<Run[]>('/api/v1/selftest')
      .then((list) => {
        const running = (list ?? []).find((r) => r.state === 'running');
        if (running) attach(running.id);
      })
      .catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [admin]);

  const attach = useCallback((id: string) => {
    after.current = 0;
    setLines([]);
    setRun({ id, profile: '', state: 'running', started: '', steps: [], lines: [], next: 0 });
  }, []);

  // Poll while a run is going.
  useEffect(() => {
    if (!run || run.state !== 'running') return;
    let live = true;
    const tick = async () => {
      try {
        const v = await api<Run>(`/api/v1/selftest/${encodeURIComponent(run.id)}?after=${after.current}`);
        if (!live) return;
        setLines((have) => mergeLines(have, v.lines ?? [], after.current));
        after.current = v.next;
        setRun(v);
        setErr('');
      } catch (e) {
        if (live) setErr(apiErrorText(e));
      }
    };
    void tick();
    const t = setInterval(tick, 1000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [run?.id, run?.state]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const el = body.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [lines]);

  async function start() {
    setBusy(true);
    setErr('');
    try {
      const r = await apiJSON<{ id: string }>('/api/v1/selftest', 'POST', { profile, noVm });
      attach(r.id);
    } catch (e) {
      setErr(apiErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  async function cancel() {
    if (!run) return;
    try {
      await api(`/api/v1/selftest/${encodeURIComponent(run.id)}`, { method: 'DELETE' });
    } catch (e) {
      setErr(apiErrorText(e));
    }
  }

  function download() {
    if (!run) return;
    const blob = new Blob([JSON.stringify({ ...run, lines }, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `kairon-selftest-${run.id}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  const planned = scenariosFor(catalogue, profile, noVm);
  const running = run?.state === 'running';
  const stepById = new Map((run?.steps ?? []).map((s) => [s.id, s]));

  if (!admin) {
    return (
      <div className="card">
        <p className="msg">The self-test creates and deletes real Machines, so it needs an administrator account.</p>
      </div>
    );
  }

  return (
    <div className="selftest">
      <aside className="card selftest-side">
        <span className="eyebrow">RUN</span>
        <label className="st-field">
          Profile
          <select value={profile} onChange={(e) => setProfile(e.target.value as Profile)} disabled={running}>
            <option value="quick">Quick</option>
            <option value="standard">Standard</option>
            <option value="full">Full</option>
          </select>
        </label>
        <p className="usageHint">{PROFILE_BLURB[profile]}</p>
        <label className="st-check">
          <input type="checkbox" checked={noVm} onChange={(e) => setNoVm(e.target.checked)} disabled={running} />
          Control plane only (no VM)
        </label>
        <div className="rowactions" style={{ justifyContent: 'flex-start', marginTop: 8 }}>
          {running ? (
            <button className="danger" onClick={cancel}>Cancel</button>
          ) : (
            <button className="primary" onClick={start} disabled={busy}>{run ? 'Run again' : 'Run self-test'}</button>
          )}
          {run && !running && <button onClick={download}>Download report</button>}
        </div>
        {err && <p className="msg error">{err}</p>}

        <span className="eyebrow" style={{ marginTop: 18 }}>CHECKS</span>
        <ul className="st-steps">
          {planned.map((sc) => {
            const s = stepById.get(sc.id);
            const st = s?.status ?? (running ? 'queued' : 'idle');
            return (
              <li key={sc.id} className={'st-step ' + st}>
                <span className="st-glyph" aria-hidden="true">{s ? stepGlyph(s.status) : '○'}</span>
                <span className="st-title">
                  {sc.title}
                  {s?.reason && <small>{s.reason}</small>}
                  {s?.hint && <small className="st-hint">hint: {s.hint}</small>}
                </span>
                <span className="st-dur">{formatDuration(s?.durationMs)}</span>
              </li>
            );
          })}
        </ul>
      </aside>

      <section className="term" aria-label="Self-test log">
        <header className="term-bar">
          <span className="dot r" /><span className="dot y" /><span className="dot g" />
          <span className="term-title">
            kaironctl selftest — {run?.profile || profile}{run?.id ? ` · run ${run.id}` : ''}
          </span>
        </header>
        <div
          className="term-body"
          ref={body}
          role="log"
          aria-live="polite"
          onScroll={(e) => {
            const el = e.currentTarget;
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
          }}
        >
          {!run && <div className="tl muted">Press “Run self-test” to boot a tiny CirrOS VM and check that everything works.</div>}
          {lines.map((l, i) => (
            <div key={i} className={'tl lv-' + l.level}>
              <span className="tl-at">{formatAt(l.atMs)}</span>
              <span className="tl-glyph">{GLYPH[l.level]}</span>
              <span className="tl-msg">{l.msg}</span>
            </div>
          ))}
          {run && !running && (
            <div className={'tl verdict ' + run.state}>
              {run.state === 'passed' ? '✓ ' : run.state === 'failed' ? '✗ ' : '! '}
              {verdict(run)}
            </div>
          )}
          {running && <div className="tl muted cursor">working…</div>}
        </div>
      </section>
    </div>
  );
}
