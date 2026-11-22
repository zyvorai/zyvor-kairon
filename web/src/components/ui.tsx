// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { ReactNode, useEffect, useRef, useState } from 'react';
import { badgeClass } from '../lib/phase';
import { countUpValue } from '../lib/countUp';

// Phase/condition pill. badgeClass() already maps every phase string the
// controllers emit to ok/progress/warn/error/idle.
export function Status({ phase }: { phase?: string }) {
  const p = phase || 'Unknown';
  return <span className={badgeClass(p)}>{p}</span>;
}

export function Meter({ value, max = 100 }: { value: number; max?: number }) {
  const pct = Math.max(0, Math.min(100, max ? (value / max) * 100 : 0));
  return (
    <span className={'meter' + (pct > 85 ? ' hot' : '')} role="meter" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
      <i style={{ width: `${pct}%` }} />
    </span>
  );
}

// Hand-rolled SVG sparkline; no chart dependency.
export function Spark({ points, width = 84, height = 28, color = 'var(--accent)' }: { points: number[]; width?: number; height?: number; color?: string }) {
  if (points.length < 2) return null;
  const min = Math.min(...points);
  const max = Math.max(...points);
  const span = max - min || 1;
  const step = width / (points.length - 1);
  const d = points
    .map((v, i) => `${i === 0 ? 'M' : 'L'}${(i * step).toFixed(1)},${(height - 3 - ((v - min) / span) * (height - 6)).toFixed(1)}`)
    .join(' ');
  return (
    <svg className="spark" width={width} height={height} viewBox={`0 0 ${width} ${height}`} aria-hidden>
      <path d={d} fill="none" stroke={color} strokeWidth={1.8} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

// useSeries keeps the last N samples of a number for sparklines.
export function useSeries(value: number | undefined, n = 24): number[] {
  const [s, setS] = useState<number[]>([]);
  useEffect(() => {
    if (value === undefined || Number.isNaN(value)) return;
    setS((prev) => [...prev, value].slice(-n));
  }, [value, n]);
  return s;
}

// useCountUp animates an integer toward `target`; honors reduced motion.
function useCountUp(target: number, durationMs = 600): number {
  const [v, setV] = useState(target);
  const prev = useRef(target);
  useEffect(() => {
    const from = prev.current;
    prev.current = target;
    const reduced = typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
    if (from === target || reduced || typeof requestAnimationFrame === 'undefined') {
      setV(target);
      return;
    }
    let raf = 0;
    const t0 = performance.now();
    const tick = (now: number) => {
      setV(countUpValue(from, target, now - t0, durationMs));
      if (now - t0 < durationMs) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [target, durationMs]);
  return v;
}

export function CountValue({ value }: { value: number }) {
  return <>{useCountUp(value)}</>;
}

export function Kpi({ label, value, sub, bad, series }: { label: string; value: ReactNode; sub?: ReactNode; bad?: boolean; series?: number[] }) {
  return (
    <div className={'kpi' + (bad ? ' bad' : '')}>
      <div className="label">{label}</div>
      <div className="value">{typeof value === 'number' ? <CountValue value={value} /> : value}</div>
      {sub && <div className="sub">{sub}</div>}
      {series && series.length > 1 && <Spark points={series} />}
    </div>
  );
}

export function KpiStrip({ children }: { children: ReactNode }) {
  return <div className="kpis">{children}</div>;
}

export function PageHero({ kicker, title, lede, glow, tone, actions }: { kicker?: string; title: string; lede?: string; glow?: boolean; tone?: string; actions?: ReactNode }) {
  return (
    <header className={'page-hero' + (glow ? ' glow' : '')} data-tone={tone}>
      {kicker && <span className="eyebrow">{kicker}</span>}
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: 16, justifyContent: 'space-between', flexWrap: 'wrap' }}>
        <div>
          <h1>{title}</h1>
          {lede && <p className="lede">{lede}</p>}
        </div>
        {actions && <div className="formactions" style={{ marginTop: 0 }}>{actions}</div>}
      </div>
    </header>
  );
}

export function EmptyState({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="empty">
      {icon}
      <h3>{title}</h3>
      {children && <p>{children}</p>}
    </div>
  );
}

// Fade-rise on first scroll into view (veyron's Reveal).
export function Reveal({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const [on, setOn] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof IntersectionObserver === 'undefined') return setOn(true);
    const io = new IntersectionObserver(([e]) => e.isIntersecting && (setOn(true), io.disconnect()), { threshold: 0.1 });
    io.observe(el);
    return () => io.disconnect();
  }, []);
  return (
    <div ref={ref} className={'reveal' + (on ? ' in' : '')}>
      {children}
    </div>
  );
}
